package main

import (
	"fmt"
	"io"
	"net/http"
	"os"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func downloadVideo(address string) (tgbotapi.FilePath, error) {
	limit := int64(originalFileLimit)
	if os.Getenv("TELEGRAM_BOT_API_URL") != "" {
		limit = 2000 * 1024 * 1024
	}
	response, err := http.Get(address)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %s", response.Status)
	}
	if response.ContentLength > limit {
		return "", fmt.Errorf("video exceeds %d MiB", limit/(1024*1024))
	}
	file, err := os.CreateTemp("", "kopoka-video-*.mp4")
	if err != nil {
		return "", err
	}
	name := file.Name()
	size, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case size > limit:
		err = fmt.Errorf("video exceeds %d MiB", limit/(1024*1024))
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return tgbotapi.FilePath(name), nil
}
