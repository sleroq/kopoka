package main

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
)

func downloadArtPhoto(client *http.Client, file tgbotapi.RequestFileData) (image.Image, error) {
	data, ok := file.(tgbotapi.FileBytes)
	if !ok {
		var err error
		data, err = downloadArtFile(client, file.(tgbotapi.FileURL))
		if err != nil {
			return nil, err
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data.Bytes))
	return img, err
}

func mergeArtPhotos(photos []interface{}) []interface{} {
	if len(photos) < 2 {
		return photos
	}
	client := &http.Client{Timeout: 10 * time.Second}
	images := make([]image.Image, len(photos))
	for i, media := range photos {
		photo := media.(tgbotapi.InputMediaPhoto)
		img, err := downloadArtPhoto(client, photo.Media)
		if err != nil {
			log.Printf("Warning: unable to merge art photos: %v", err)
			return photos
		}
		images[i] = img
	}

	var merged []interface{}
	for _, panel := range joinArtPanels(images) {
		if panel.first == panel.last {
			merged = append(merged, photos[panel.first])
			continue
		}
		img := panel.image
		size := img.Bounds().Size()
		if total := size.X + size.Y; total > 10000 {
			// Telegram requires width+height <= 10000. Resize the complete artwork,
			// not individual panels, so the seams stay aligned.
			bounds := image.Rect(0, 0, size.X*10000/total, size.Y*10000/total)
			resized := image.NewRGBA(bounds)
			draw.CatmullRom.Scale(resized, bounds, img, img.Bounds(), draw.Src, nil)
			img = resized
		}
		var data bytes.Buffer
		err := jpeg.Encode(&data, img, &jpeg.Options{Quality: 95})
		if err != nil {
			log.Printf("Warning: unable to encode merged art photos: %v", err)
			return photos
		}
		if data.Len() > 10*1024*1024 {
			merged = append(merged, photos[panel.first:panel.last+1]...)
			continue
		}
		photo := photos[panel.first].(tgbotapi.InputMediaPhoto)
		photo.Media = tgbotapi.FileBytes{Name: "joined-art.jpg", Bytes: data.Bytes()}
		merged = append(merged, photo)
	}
	return merged
}

func sendPhotos(bot *tgbotapi.BotAPI, chatID int64, replyID int, photos []interface{}) error {
	if len(photos) == 0 {
		return nil
	}
	original := photos[0].(tgbotapi.InputMediaPhoto)
	for start := 0; start < len(photos); start += 10 {
		end := min(start+10, len(photos))
		batch := make([]interface{}, 0, end-start)
		for i := start; i < end; i++ {
			photo := photos[i].(tgbotapi.InputMediaPhoto)
			photo.Caption = ""
			photo.ParseMode = ""
			photo.CaptionEntities = nil
			if i == start {
				photo.Caption = original.Caption
				photo.ParseMode = original.ParseMode
				photo.CaptionEntities = original.CaptionEntities
			}
			batch = append(batch, photo)
		}
		if len(batch) == 1 {
			media := batch[0].(tgbotapi.InputMediaPhoto)
			photo := tgbotapi.NewPhoto(chatID, media.Media)
			photo.ReplyToMessageID = replyID
			photo.Caption = media.Caption
			photo.ParseMode = media.ParseMode
			photo.CaptionEntities = media.CaptionEntities
			if _, err := bot.Send(photo); err != nil {
				return err
			}
		} else {
			group := tgbotapi.NewMediaGroup(chatID, batch)
			group.ReplyToMessageID = replyID
			if _, err := bot.SendMediaGroup(group); err != nil {
				return err
			}
		}
	}
	return nil
}
