package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
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

func init() {
	connStr := os.Getenv("POSTGRES_URL")
	if connStr != "" {
		db, _ = sql.Open("postgres", connStr)
		if db != nil {
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

	if update.CallbackQuery != nil {
		chatID := update.CallbackQuery.Message.Chat.ID
		data := update.CallbackQuery.Data

		bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, ""))

		var artist, title string
		var cover []byte
		db.QueryRow("SELECT artist, title, cover FROM user_settings WHERE chat_id = $1", chatID).Scan(&artist, &title, &cover)

		switch data {
		case "settings":
			saveUserState(chatID, 1, artist, title, cover)
			msg := tgbotapi.NewMessage(chatID, "✨ **بدء ضبط الإعدادات**\n\n🎤 أرسل لي الآن **اسم الفنان**:")
			msg.ReplyMarkup = cancelKeyboard()
			bot.Send(msg)
		case "cancel":
			saveUserState(chatID, 4, artist, title, cover)
			msg := tgbotapi.NewMessage(chatID, "❌ **تم إلغاء العملية.**\nتم الحفاظ على إعداداتك السابقة.")
			bot.Send(msg)
		case "status":
			if artist == "" {
				bot.Send(tgbotapi.NewMessage(chatID, "⚠️ لم تقم بضبط أي إعدادات بعد."))
			} else {
				msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("✅ **إعداداتك الحالية:**\n\n🎤 الفنان: %s\n🎵 العنوان: %s\n\nأرسل أي ملف صوتي لتطبيقها فوراً.", artist, title))
				bot.Send(msg)
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if update.Message == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	chatID := update.Message.Chat.ID
	text := update.Message.Text

	var step int
	var artist, title string
	var cover []byte
	err = db.QueryRow("SELECT step, artist, title, cover FROM user_settings WHERE chat_id = $1", chatID).Scan(&step, &artist, &title, &cover)
	if err == sql.ErrNoRows {
		step = 0
	}

	if update.Message.IsCommand() {
		switch update.Message.Command() {
		case "start", "menu":
			sendColoredMainMenu(botToken, chatID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if step == 1 && text != "" {
		saveUserState(chatID, 2, text, title, cover)
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("✅ تم حفظ الفنان: **%s**\n\n🎵 أرسل الآن **اسم الملف الصوتي (العنوان)**:", text))
		msg.ReplyMarkup = cancelKeyboard()
		bot.Send(msg)
		w.WriteHeader(http.StatusOK)
		return
	} else if step == 2 && text != "" {
		saveUserState(chatID, 3, artist, text, cover)
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("✅ تم حفظ العنوان: **%s**\n\n🖼 أرسل الآن **الصورة المصغرة** (سيتم قص المنتصف تلقائياً):", text))
		msg.ReplyMarkup = cancelKeyboard()
		bot.Send(msg)
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
			bot.Send(tgbotapi.NewMessage(chatID, "🎉 **تم حفظ جميع الإعدادات بنجاح!**\n\nالآن قم بتحويل أو إرسال أي ملف MP3، وسأقوم بتعديله لك فوراً."))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if update.Message.Audio != nil || update.Message.Document != nil {
		if step != 4 || artist == "" {
			bot.Send(tgbotapi.NewMessage(chatID, "⚠️ يجب إكمال الإعدادات أولاً. استخدم الأزرار للبدء."))
			sendColoredMainMenu(botToken, chatID)
			w.WriteHeader(http.StatusOK)
			return
		}

		bot.Send(tgbotapi.NewMessage(chatID, "⏳ جاري استخراج الصوت وتطبيق الإعدادات..."))

		var fileID string
		if update.Message.Audio != nil {
			fileID = update.Message.Audio.FileID
		} else {
			fileID = update.Message.Document.FileID
		}

		fileURL, err := bot.GetFileDirectURL(fileID)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ عذراً، الملف كبير جداً أو هناك خطأ في تيليجرام."))
			w.WriteHeader(http.StatusOK)
			return
		}

		audioData, err := downloadFile(fileURL)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ فشل تحميل الملف الصوتي."))
			w.WriteHeader(http.StatusOK)
			return
		}

		// استخراج وإرسال الصورة القديمة 
		reader := bytes.NewReader(audioData)
		tag, err := id3v2.ParseReader(reader, id3v2.Options{Parse: true})
		
		if err == nil && tag != nil {
			pictures := tag.GetFrames(tag.CommonID("Attached picture"))
			if len(pictures) > 0 {
				if pic, ok := pictures[0].(id3v2.PictureFrame); ok {
					picMsg := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: "old_cover.jpg", Bytes: pic.Picture})
					picMsg.Caption = "🖼 الصورة المصغرة الأصلية للملف."
					bot.Send(picMsg)
				}
			}
		}

		// استخدام الدالة الجديدة كلياً التي تدمج الموسيقى يدوياً في الذاكرة
		editedAudio, err := editAudioTagsInMemory(audioData, artist, title, cover)
		if err != nil {
			bot.Send(tgbotapi.NewMessage(chatID, "❌ خطأ في دمج البيانات."))
			w.WriteHeader(http.StatusOK)
			return
		}

		audioMsg := tgbotapi.NewAudio(chatID, tgbotapi.FileBytes{Name: title + ".mp3", Bytes: editedAudio})
		audioMsg.Performer = artist
		audioMsg.Title = title
		bot.Send(audioMsg)

		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func cancelKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ إلغاء الإعداد", "cancel"),
		),
	)
}

func sendColoredMainMenu(botToken string, chatID int64) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       "🎧 **مرحباً بك في بوت تعديل الصوتيات!**\n\nيُرجى اختيار إجراء من القائمة أدناه:",
		"parse_mode": "Markdown",
		"reply_markup": map[string]interface{}{
			"inline_keyboard": [][]map[string]interface{}{
				{
					{"text": "⚙️ إعداد صورة واسم جديد للملف", "callback_data": "settings", "style": "primary"},
				},
				{
					{"text": "ℹ️ عرض الإعدادات الحالية", "callback_data": "status"},
				},
				{
					{"text": "🗑 إلغاء", "callback_data": "cancel", "style": "danger"},
				},
			},
		},
	}
	jsonPayload, _ := json.Marshal(payload)
	http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
}

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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ----------------------------------------------------
// الحل الجذري: دمج البيانات والموسيقى يدوياً داخل الذاكرة
// ----------------------------------------------------
func editAudioTagsInMemory(originalAudio []byte, artist, title string, cover []byte) ([]byte, error) {
	reader := bytes.NewReader(originalAudio)
	
	// 1. قراءة الملف الأصلي لتحديد مساحة الإعدادات القديمة
	_, err := id3v2.ParseReader(reader, id3v2.Options{Parse: true})
	
	audioDataOffset := int64(0)
	if err == nil {
		// حساب حجم الإعدادات القديمة بالضبط
		audioDataOffset = reader.Size() - int64(reader.Len())
	}

	// حماية إضافية
	if audioDataOffset < 0 || audioDataOffset > int64(len(originalAudio)) {
		audioDataOffset = 0
	}

	// 2. قص الموسيقى الصافية فقط (بدون التخريب القديم)
	rawAudio := originalAudio[audioDataOffset:]

	// 3. تصميم الإعدادات الجديدة 
	newTag := id3v2.NewEmptyTag()
	newTag.SetArtist(artist)
	newTag.SetTitle(title)

	if len(cover) > 0 {
		pic := id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    "image/jpeg",
			PictureType: id3v2.PTFrontCover,
			Description: "Cover",
			Picture:     cover,
		}
		newTag.AddAttachedPicture(pic)
	}

	// 4. كتابة الإعدادات الجديدة + لصق الموسيقى الصافية خلفها مباشرة
	var buf bytes.Buffer
	if _, err := newTag.WriteTo(&buf); err != nil {
		return nil, err
	}
	
	// هنا يكمن السر: ندمج الموسيقى يدوياً لضمان عدم ضياعها!
	buf.Write(rawAudio) 

	return buf.Bytes(), nil
}
