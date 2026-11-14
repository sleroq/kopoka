package main

import (
	"errors"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
