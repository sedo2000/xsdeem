package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/bogem/id3v2/v2"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
)

// هيكل لحفظ حالة المستخدم وإعداداته (يفضل استبدالها بـ Redis لاحقاً)
type UserState struct {
	Step       string // "idle", "waiting_artist", "waiting_title", "waiting_cover"
	Artist     string
	Title      string
	CoverBytes []byte
}

var userStates = make(map[int64]*UserState)

func Handler(w http.ResponseWriter, r *http.Request) {
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

	if update.Message != nil {
		chatID := update.Message.Chat.ID

		// تهيئة حالة المستخدم إذا لم تكن موجودة
		if _, exists := userStates[chatID]; !exists {
			userStates[chatID] = &UserState{Step: "idle"}
		}
		state := userStates[chatID]

		// التعامل مع الأوامر
		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				msg := tgbotapi.NewMessage(chatID, "مرحباً بك في بوت تعديل الصوتيات 🎵\nأرسل ملف صوتي لاستخراج صورته، أو استخدم /settings لإعداد الصورة المصغرة واسم الفنان.")
				bot.Send(msg)
			case "settings":
				state.Step = "waiting_artist"
				bot.Send(tgbotapi.NewMessage(chatID, "لنقم بضبط الإعدادات.\nأرسل الآن **اسم الفنان**: "))
			}
			return
		}

		// التعامل مع تدفق المحادثة (State Machine)
		switch state.Step {
		case "waiting_artist":
			state.Artist = update.Message.Text
			state.Step = "waiting_title"
			bot.Send(tgbotapi.NewMessage(chatID, "تم الحفظ. أرسل الآن **اسم الملف الصوتي (العنوان)**: "))
			return
		case "waiting_title":
			state.Title = update.Message.Text
			state.Step = "waiting_cover"
			bot.Send(tgbotapi.NewMessage(chatID, "تم الحفظ. أرسل الآن **الصورة المصغرة** (سيتم التركيز على منتصفها تلقائياً): "))
			return
		case "waiting_cover":
			if len(update.Message.Photo) > 0 {
				// أخذ أعلى جودة للصورة
				photo := update.Message.Photo[len(update.Message.Photo)-1]
				fileURL, _ := bot.GetFileDirectURL(photo.FileID)
				
				// تحميل الصورة وقصها من المنتصف
				croppedImg, err := processAndCropImage(fileURL)
				if err == nil {
					state.CoverBytes = croppedImg
					state.Step = "idle"
					bot.Send(tgbotapi.NewMessage(chatID, "✅ تم حفظ جميع الإعدادات! أرسل الآن أي ملف صوتي لتعديله."))
				} else {
					bot.Send(tgbotapi.NewMessage(chatID, "❌ حدث خطأ في معالجة الصورة، حاول مرة أخرى."))
				}
			}
			return
		}

		// التعامل مع الملفات الصوتية المُرسلة
		if update.Message.Audio != nil {
			audio := update.Message.Audio
			fileURL, _ := bot.GetFileDirectURL(audio.FileID)
			
			// تحميل الملف الصوتي للذاكرة
			resp, err := http.Get(fileURL)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			audioData, _ := ioutil.ReadAll(resp.Body)

			// 1. استخراج الصورة المصغرة الحالية (إذا طلب المستخدم ذلك)
			tag, err := id3v2.ParseReader(bytes.NewReader(audioData), id3v2.Options{Parse: true})
			if err == nil {
				pictures := tag.AttachedPictures()
				if len(pictures) > 0 {
					// إرسال الصورة المصغرة القديمة للمستخدم
					photoMsg := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: "cover.jpg", Bytes: pictures[0].Picture})
					photoMsg.Caption = "🖼 هذه هي الصورة المصغرة الأصلية للملف الصوتي."
					bot.Send(photoMsg)
				}
			}

			// 2. تعديل الملف الصوتي بناءً على الإعدادات المحفوظة
			if state.Artist != "" && state.CoverBytes != nil {
				editedAudio, err := editAudioTags(audioData, state)
				if err == nil {
					// إرسال الملف المُعدل
					audioMsg := tgbotapi.NewAudio(chatID, tgbotapi.FileBytes{Name: state.Title + ".mp3", Bytes: editedAudio})
					audioMsg.Performer = state.Artist
					audioMsg.Title = state.Title
					bot.Send(audioMsg)
				}
			} else {
				bot.Send(tgbotapi.NewMessage(chatID, "⚠️ لم تقم بضبط الإعدادات بعد. استخدم /settings أولاً."))
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// دالة لمعالجة الصورة وقصها لتكون مربعة من المنتصف (Center Crop)
func processAndCropImage(url string) ([]byte, error) {
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
	width, height := bounds.Dx(), bounds.Dy()
	
	// إيجاد الحجم الأصغر لجعله مربعاً
	size := width
	if height < width {
		size = height
	}

	startX := (width - size) / 2
	startY := (height - size) / 2

	rect := image.Rect(0, 0, size, size)
	dst := image.NewRGBA(rect)
	
	// رسم الجزء الأوسط فقط من الصورة
	draw.Draw(dst, rect, img, image.Point{X: bounds.Min.X + startX, Y: bounds.Min.Y + startY}, draw.Src)

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90})
	return buf.Bytes(), err
}

// دالة لتعديل الـ Metadata للملف الصوتي
func editAudioTags(originalAudio []byte, state *UserState) ([]byte, error) {
	tag, err := id3v2.ParseReader(bytes.NewReader(originalAudio), id3v2.Options{Parse: true})
	if err != nil {
		tag = id3v2.NewEmptyTag()
	}

	// تعيين اسم الفنان والملف
	tag.SetArtist(state.Artist)
	tag.SetTitle(state.Title)

	// تعيين الصورة المصغرة الجديدة
	pic := id3v2.PictureFrame{
		Encoding:    id3v2.EncodingUTF8,
		MimeType:    "image/jpeg",
		PictureType: id3v2.PTFrontCover,
		Description: "Cover",
		Picture:     state.CoverBytes,
	}
	tag.AddAttachedPicture(pic)

	// كتابة الملف الجديد للذاكرة
	var buf bytes.Buffer
	_, err = tag.WriteTo(&buf)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
