package main

import (
	"encoding/json"
	"errors"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// The Telegram library predates rich messages, so retain their captions on updates.
type telegramMessage struct {
	tgbotapi.Message
	RichMessage struct {
		Blocks []struct {
			Caption struct {
				Text json.RawMessage `json:"text"`
			} `json:"caption"`
		} `json:"blocks"`
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
