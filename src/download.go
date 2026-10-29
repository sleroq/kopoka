package main

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func downloadArtFile(client *http.Client, file tgbotapi.FileURL) (tgbotapi.FileBytes, error) {
	req, err := http.NewRequest(http.MethodGet, string(file), nil)
	if err != nil {
		return tgbotapi.FileBytes{}, err
	}
	if req.URL.Hostname() == "pximg.net" || strings.HasSuffix(req.URL.Hostname(), ".pximg.net") {
		req.Header.Set("Referer", "https://www.pixiv.net/")
	}
	res, err := client.Do(req)
	if err != nil {
		return tgbotapi.FileBytes{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return tgbotapi.FileBytes{}, fmt.Errorf("downloading art: HTTP %s", res.Status)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return tgbotapi.FileBytes{}, err
	}
	return tgbotapi.FileBytes{Name: path.Base(req.URL.Path), Bytes: data}, nil
}
