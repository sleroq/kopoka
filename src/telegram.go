package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// The Telegram library predates rich messages, so retain their text on updates.
type telegramRichBlock struct {
	Text    json.RawMessage `json:"text"`
	Caption struct {
		Text json.RawMessage `json:"text"`
	} `json:"caption"`
	Blocks []telegramRichBlock `json:"blocks"`
}

type telegramMessage struct {
	tgbotapi.Message
	RichMessage struct {
		Blocks []telegramRichBlock `json:"blocks"`
	} `json:"rich_message"`
}

type telegramUpdate struct {
	tgbotapi.Update
	Message     *telegramMessage `json:"message"`
	ChannelPost *telegramMessage `json:"channel_post"`
}

func getTelegramUpdates(bot *tgbotapi.BotAPI, config tgbotapi.UpdateConfig) ([]telegramUpdate, error) {
	response, err := bot.Request(config)
	if err != nil {
		return nil, err
	}
	var updates []telegramUpdate
	err = json.Unmarshal(response.Result, &updates)
	return updates, err
}

func telegramUpdates(bot *tgbotapi.BotAPI, config tgbotapi.UpdateConfig) <-chan telegramUpdate {
	updates := make(chan telegramUpdate, bot.Buffer)
	go func() {
		for {
			batch, err := getTelegramUpdates(bot, config)
			if err != nil {
				log.Printf("getting Telegram updates: %v; retrying in 3 seconds", err)
				time.Sleep(3 * time.Second)
				continue
			}
			for _, update := range batch {
				if update.UpdateID >= config.Offset {
					config.Offset = update.UpdateID + 1
					updates <- update
				}
			}
		}
	}()
	return updates
}

func retryTelegram[T any](operation func() (T, error)) (T, error) {
	for {
		result, err := operation()
		var telegramErr *tgbotapi.Error
		if !errors.As(err, &telegramErr) || (telegramErr.Code != 429 && telegramErr.Code != 0) || telegramErr.RetryAfter <= 0 {
			return result, err
		}
		time.Sleep(time.Duration(telegramErr.RetryAfter) * time.Second)
	}
}

func newTelegramBot(token string) (*tgbotapi.BotAPI, error) {
	base := os.Getenv("TELEGRAM_BOT_API_URL")
	if base == "" {
		return tgbotapi.NewBotAPI(token)
	}
	return tgbotapi.NewBotAPIWithAPIEndpoint(token, strings.TrimRight(base, "/")+"/bot%s/%s")
}

func streamingVideo(chatID int64, media tgbotapi.InputMediaVideo) tgbotapi.VideoConfig {
	video := tgbotapi.NewVideo(chatID, media.Media)
	video.Thumb = media.Thumb
	video.Duration = media.Duration
	video.Caption = media.Caption
	video.ParseMode = media.ParseMode
	video.CaptionEntities = media.CaptionEntities
	video.SupportsStreaming = true
	return video
}

func telegramURLFailure(err error) bool {
	// UploadFiles omits the error code even when Telegram returns one.
	var telegramErr *tgbotapi.Error
	return errors.As(err, &telegramErr) && (telegramErr.Code == 400 || telegramErr.Code == 0) &&
		(strings.Contains(telegramErr.Message, "failed to get HTTP URL content") || strings.Contains(telegramErr.Message, "WEBPAGE_CURL_FAILED"))
}
