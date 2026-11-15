package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
	"golang.org/x/image/vector"
	_ "golang.org/x/image/webp"
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

func mergeArtPhotos(photos []any) []any {
	if len(photos) < 2 {
		return photos
	}
	for _, item := range photos {
		if _, ok := item.(tgbotapi.InputMediaVideo); ok {
			var result []any
			for start := 0; start < len(photos); {
				if _, ok := photos[start].(tgbotapi.InputMediaVideo); ok {
					result = append(result, photos[start])
					start++
					continue
				}

				end := start + 1
				for end < len(photos) {
					if _, ok := photos[end].(tgbotapi.InputMediaPhoto); !ok {
						break
					}
					end++
				}
				result = append(result, mergeArtPhotos(photos[start:end])...)
				start = end
			}
			return result
		}
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

	var merged []any
	for _, panel := range joinArtPanels(images) {
		if panel.first == panel.last {
			merged = append(merged, photos[panel.first])
			continue
		}
		file, err := mergedPhotoFile(panel.image)
		if err != nil {
			log.Printf("Warning: unable to encode merged art photos: %v", err)
			return photos
		}
		if file.Bytes == nil {
			merged = append(merged, photos[panel.first:panel.last+1]...)
			continue
		}
		photo := photos[panel.first].(tgbotapi.InputMediaPhoto)
		photo.Media = file
		merged = append(merged, photo)
	}
	return merged
}

func forceMergePanels(photos []any, bars bool) ([]any, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	result := make([]any, 0, len(photos))

	for start := 0; start < len(photos); {
		if _, ok := photos[start].(tgbotapi.InputMediaPhoto); !ok {
			result = append(result, photos[start])
			start++
			continue
		}
		end := start + 1
		for end < len(photos) {
			if _, ok := photos[end].(tgbotapi.InputMediaPhoto); !ok {
				break
			}
			end++
		}
		if end == start+1 {
			result = append(result, photos[start])
			start = end
			continue
		}

		images := make([]image.Image, 0, end-start)
		width, height := 0, 0
		for _, item := range photos[start:end] {
			img, err := downloadArtPhoto(client, item.(tgbotapi.InputMediaPhoto).Media)
			if err != nil {
				return nil, fmt.Errorf("downloading photo for merge: %w", err)
			}
			images = append(images, img)
			height = max(height, img.Bounds().Dy())
		}
		for i, img := range images {
			size := img.Bounds().Size()
			if bars && size.Y != height {
				bounds := image.Rect(0, 0, max(1, (size.X*height+size.Y/2)/size.Y), height)
				resized := image.NewRGBA(bounds)
				draw.CatmullRom.Scale(resized, bounds, img, img.Bounds(), draw.Src, nil)
				images[i] = resized
			}
			width += images[i].Bounds().Dx()
		}

		gap := 0
		background := color.White
		if bars {
			gap = max(1, height*8/905)
			width += gap * (len(images) - 1)
			background = color.Black
		}
		canvas := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
		x := 0
		for _, img := range images {
			size := img.Bounds().Size()
			if bars {
				radius := float32(min(max(1, size.Y*12/905), min(size.X, size.Y)/2))
				mask := image.NewAlpha(image.Rect(0, 0, size.X, size.Y))
				z := vector.NewRasterizer(size.X, size.Y)
				w, h := float32(size.X), float32(size.Y)
				z.MoveTo(radius, 0)
				z.LineTo(w-radius, 0)
				z.QuadTo(w, 0, w, radius)
				z.LineTo(w, h-radius)
				z.QuadTo(w, h, w-radius, h)
				z.LineTo(radius, h)
				z.QuadTo(0, h, 0, h-radius)
				z.LineTo(0, radius)
				z.QuadTo(0, 0, radius, 0)
				z.ClosePath()
				z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
				draw.DrawMask(canvas, image.Rect(x, 0, x+size.X, size.Y), img, img.Bounds().Min, mask, image.Point{}, draw.Over)
			} else {
				draw.Draw(canvas, image.Rect(x, 0, x+size.X, size.Y), img, img.Bounds().Min, draw.Over)
			}
			x += size.X + gap
		}

		file, err := mergedPhotoFile(canvas)
		if err != nil {
			return nil, fmt.Errorf("encoding merged photos: %w", err)
		}
		if file.Bytes == nil {
			return nil, fmt.Errorf("merged photo exceeds Telegram's 10 MB limit")
		}

		photo := photos[start].(tgbotapi.InputMediaPhoto)
		photo.Media = file
		result = append(result, photo)
		start = end
	}
	return result, nil
}

func mergedPhotoFile(img image.Image) (tgbotapi.FileBytes, error) {
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
	if err := jpeg.Encode(&data, img, &jpeg.Options{Quality: 95}); err != nil {
		return tgbotapi.FileBytes{}, err
	}
	if data.Len() > 10*1024*1024 {
		return tgbotapi.FileBytes{}, nil
	}
	return tgbotapi.FileBytes{Name: "joined-art.jpg", Bytes: data.Bytes()}, nil
}

func sendPhotos(bot *tgbotapi.BotAPI, chatID int64, replyID int, photos []any) error {
	_, err := sendPhotosWithIDs(bot, chatID, replyID, photos)
	return err
}

func mediaBase(item any) tgbotapi.BaseInputMedia {
	switch media := item.(type) {
	case tgbotapi.InputMediaPhoto:
		return media.BaseInputMedia
	case tgbotapi.InputMediaVideo:
		return media.BaseInputMedia
	case tgbotapi.InputMediaAnimation:
		return media.BaseInputMedia
	default:
		panic("unsupported media")
	}
}

func sendPhotosWithIDs(bot *tgbotapi.BotAPI, chatID int64, replyID int, photos []any) ([]string, error) {
	if len(photos) == 0 {
		return nil, nil
	}
	var ids []string
	original := mediaBase(photos[0])

	for start := 0; start < len(photos); start += 10 {
		end := min(start+10, len(photos))
		batch := make([]any, 0, end-start)
		for i := start; i < end; i++ {
			base := mediaBase(photos[i])
			base.Caption = ""
			base.ParseMode = ""
			base.CaptionEntities = nil
			if i == start {
				base.Caption = original.Caption
				base.ParseMode = original.ParseMode
				base.CaptionEntities = original.CaptionEntities
			}
			switch item := photos[i].(type) {
			case tgbotapi.InputMediaPhoto:
				item.BaseInputMedia = base
				batch = append(batch, item)
			case tgbotapi.InputMediaVideo:
				item.BaseInputMedia = base
				batch = append(batch, item)
			case tgbotapi.InputMediaAnimation:
				item.BaseInputMedia = base
				batch = append(batch, item)
			}
		}

		messages, err := sendPhotoBatchWithVideoRetry(bot, chatID, replyID, batch)
		if err != nil {
			return nil, err
		}

		for _, message := range messages {
			switch {
			case message.Animation != nil && message.Animation.FileID != "":
				ids = append(ids, message.Animation.FileID)
			case message.Video != nil && message.Video.FileID != "":
				ids = append(ids, message.Video.FileID)
			case len(message.Photo) > 0 && message.Photo[len(message.Photo)-1].FileID != "":
				ids = append(ids, message.Photo[len(message.Photo)-1].FileID)
			}
		}
	}
	return ids, nil
}

func sendPhotoBatchWithVideoRetry(bot *tgbotapi.BotAPI, chatID int64, replyID int, batch []any) ([]tgbotapi.Message, error) {
	messages, err := sendPhotoBatch(bot, chatID, replyID, batch)
	var telegramErr *tgbotapi.Error
	if !errors.As(err, &telegramErr) || telegramErr.Code != 400 || !strings.Contains(telegramErr.Message, "failed to get HTTP URL content") {
		return messages, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	downloaded := false
	for i, item := range batch {
		video, ok := item.(tgbotapi.InputMediaVideo)
		if !ok {
			continue
		}
		url, ok := video.Media.(tgbotapi.FileURL)
		if !ok {
			continue
		}
		file, downloadErr := downloadOriginal(client, string(url), false)
		if downloadErr != nil {
			return nil, downloadErr
		}
		video.Media = file
		batch[i] = video
		downloaded = true
	}
	if downloaded {
		return sendPhotoBatch(bot, chatID, replyID, batch)
	}
	return messages, err
}

func sendPhotoBatch(bot *tgbotapi.BotAPI, chatID int64, replyID int, batch []any) ([]tgbotapi.Message, error) {
	var messages []tgbotapi.Message
	if len(batch) == 1 {
		base := mediaBase(batch[0])
		var msg tgbotapi.Message
		var err error
		switch base.Type {
		case "animation":
			animation := tgbotapi.NewAnimation(chatID, base.Media)
			animation.ReplyToMessageID = replyID
			animation.Caption = base.Caption
			animation.ParseMode = base.ParseMode
			animation.CaptionEntities = base.CaptionEntities
			msg, err = retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(animation) })
		case "video":
			video := tgbotapi.NewVideo(chatID, base.Media)
			video.ReplyToMessageID = replyID
			video.Caption = base.Caption
			video.ParseMode = base.ParseMode
			video.CaptionEntities = base.CaptionEntities
			msg, err = retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(video) })
		default:
			photo := tgbotapi.NewPhoto(chatID, base.Media)
			photo.ReplyToMessageID = replyID
			photo.Caption = base.Caption
			photo.ParseMode = base.ParseMode
			photo.CaptionEntities = base.CaptionEntities
			msg, err = retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(photo) })
		}
		if err != nil {
			return nil, err
		}
		messages = []tgbotapi.Message{msg}
	} else {
		group := tgbotapi.NewMediaGroup(chatID, batch)
		group.ReplyToMessageID = replyID
		var err error
		messages, err = retryTelegram(func() ([]tgbotapi.Message, error) { return bot.SendMediaGroup(group) })
		if err != nil {
			return nil, err
		}
	}

	return messages, nil
}
