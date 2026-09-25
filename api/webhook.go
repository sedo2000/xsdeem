package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"

	"github.com/bogem/id3v2/v2"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/lib/pq"
	"golang.org/x/image/draw"
)

var db *sql.DB

// دالة التهيئة تعمل تلقائياً للاتصال بقاعدة البيانات وإنشاء الجدول
func init() {
	var err error
	connStr := os.Getenv("POSTGRES_URL")
	if connStr != "" {
		db, err = sql.Open("postgres", connStr)
		if err == nil {
			createTable := `
			CREATE TABLE IF NOT EXISTS user_settings (
				chat_id BIGINT PRIMARY KEY,
				step INT,
				artist TEXT,
				title TEXT,
				cover BYTEA
			);`
			db.Exec(createTable)
		}
	}
}

func Handler(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "Database Connection Error", http.StatusInternalServerError)
		return
	}

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		http.Error(w, "Bot Token Error", http.StatusInternalServerError)
		return
	}

	var update tgbotapi.Update
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	if update.Message == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	chatID := update.Message.Chat.ID
	text := update.Message.Text

	// استخراج حالة المستخدم من قاعدة البيانات
	var step int
	var artist, title string
	var cover []byte

	err = db.QueryRow("SELECT step, artist, title, cover FROM user_settings WHERE chat_id = $1", chatID).Scan(&step, &artist, &title, &cover)
	if err == sql.ErrNoRows {
		step = 0 // مستخدم جديد
	}

	// 1. التعامل مع الأوامر
	if update.Message.IsCommand() {
		switch update.Message.Command() {
		case "start", "settings":
			saveUserState(chatID, 1, "", "", nil)
			bot.Send(tgbotapi.NewMessage(chatID, "مرحباً! لنقم بضبط الإعدادات ⚙️\n\nأرسل لي الآن **اسم الفنان**: "))
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// 2. التعامل مع خطوات إعداد البوت
	if step == 1 && text != "" {
		saveUserState(chatID, 2, text, title, cover)
		bot.Send(tgbotapi.NewMessage(chatID, "تم حفظ الفنان ✅\nأرسل الآن **اسم الملف الصوتي (العنوان)**: "))
		w.WriteHeader(http.StatusOK)
		return
	} else if step == 2 && text != "" {
		saveUserState(chatID, 3, artist, text, cover)
		bot.Send(tgbotapi.NewMessage(chatID, "تم حفظ العنوان ✅\nأرسل الآن **الصورة المصغرة** (سيتم قص المنتصف تلقائياً): "))
		w.WriteHeader(http.StatusOK)
		return
	} else if step == 3 && len(update.Message.Photo) > 0 {
		photo := update.Message.Photo[len(update.Message.Photo)-1]
		fileURL, _ := bot.GetFileDirectURL(photo.FileID)

		croppedImg, err := cropImageFromCenter(fileURL)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ حدث خطأ أثناء قص الصورة. حاول إرسال صورة أخرى."))
		} else {
			saveUserState(chatID, 4, artist, title, croppedImg)
			bot.Send(tgbotapi.NewMessage(chatID, "✅ **تم حفظ الإعدادات بنجاح في قاعدة البيانات!**\nالآن أرسل أي ملف صوتي وسأقوم بتعديله فوراً."))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// 3. التعامل مع الملفات الصوتية المُرسلة
	if update.Message.Audio != nil || update.Message.Document != nil {
		if step != 4 {
			bot.Send(tgbotapi.NewMessage(chatID, "⚠️ يجب إكمال الإعدادات أولاً. أرسل /settings للبدء."))
			w.WriteHeader(http.StatusOK)
			return
		}

		bot.Send(tgbotapi.NewMessage(chatID, "⏳ جاري المعالجة..."))

		var fileID string
		if update.Message.Audio != nil {
			fileID = update.Message.Audio.FileID
		} else {
			fileID = update.Message.Document.FileID
		}

		fileURL, _ := bot.GetFileDirectURL(fileID)
		audioData, err := downloadFile(fileURL)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ فشل تحميل الملف الصوتي."))
			w.WriteHeader(http.StatusOK)
			return
		}

		// استخراج الصورة القديمة
		tag, err := id3v2.ParseReader(bytes.NewReader(audioData), id3v2.Options{Parse: true})
		if err == nil {
			pictures := tag.AttachedPictures()
			if len(pictures) > 0 {
				picMsg := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: "old_cover.jpg", Bytes: pictures[0].Picture})
				picMsg.Caption = "🖼 الصورة المصغرة الأصلية."
				bot.Send(picMsg)
			}
		}

		// تعديل الملف
		editedAudio, err := editAudioTags(audioData, artist, title, cover)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ خطأ في دمج البيانات."))
			w.WriteHeader(http.StatusOK)
			return
		}

		// إرسال الملف المعدل
		audioMsg := tgbotapi.NewAudio(chatID, tgbotapi.FileBytes{Name: title + ".mp3", Bytes: editedAudio})
		audioMsg.Performer = artist
		audioMsg.Title = title
		bot.Send(audioMsg)

		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// دالة لحفظ وتحديث بيانات المستخدم في قاعدة البيانات
func saveUserState(chatID int64, step int, artist, title string, cover []byte) {
	if cover == nil {
		cover = []byte{}
	}
	query := `
		INSERT INTO user_settings (chat_id, step, artist, title, cover)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chat_id) 
		DO UPDATE SET step = $2, artist = $3, title = $4, cover = $5;
	`
	db.Exec(query, chatID, step, artist, title, cover)
}

func cropImageFromCenter(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	size := w
	if h < w {
		size = h
	}

	startX := (w - size) / 2
	startY := (h - size) / 2

	rect := image.Rect(0, 0, size, size)
	dst := image.NewRGBA(rect)

	draw.Draw(dst, rect, img, image.Point{X: bounds.Min.X + startX, Y: bounds.Min.Y + startY}, draw.Src)

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 95})
	return buf.Bytes(), err
}

func downloadFile(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func editAudioTags(originalAudio []byte, artist, title string, cover []byte) ([]byte, error) {
	tag, err := id3v2.ParseReader(bytes.NewReader(originalAudio), id3v2.Options{Parse: true})
	if err != nil {
		tag = id3v2.NewEmptyTag()
	}

	tag.SetArtist(artist)
	tag.SetTitle(title)

	if len(cover) > 0 {
		pic := id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    "image/jpeg",
			PictureType: id3v2.PTFrontCover,
			Description: "Cover",
			Picture:     cover,
		}
		tag.AddAttachedPicture(pic)
	}

	var buf bytes.Buffer
	if _, err = tag.WriteTo(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
