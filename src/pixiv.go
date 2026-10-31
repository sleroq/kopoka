package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func downloadPixivPhoto(client *http.Client, originalURL, largeURL string) (tgbotapi.FileBytes, error) {
	// Upload locally: Telegram cannot fetch Pixiv's hotlink-protected URLs.
	const maxPhotoSize = 10 * 1024 * 1024
	for _, imageURL := range []string{originalURL, largeURL} {
		if imageURL == "" {
			continue
		}
		file, err := downloadArtFile(client, tgbotapi.FileURL(imageURL))
		if err != nil {
			return file, fmt.Errorf("downloading Pixiv image: %w", err)
		}
		if len(file.Bytes) <= maxPhotoSize {
			return file, nil
		}
	}
	return tgbotapi.FileBytes{}, fmt.Errorf("no Pixiv image variant fits Telegram's 10 MB photo limit")
}

func setupPixivClient() (*pixiv.AppPixivAPI, error) {
	tokenDeadlineSec, err := strconv.Atoi(os.Getenv("PIXIV_TOKEN_DEADLINE"))
	if err != nil {
		return nil, fmt.Errorf("parsing PIXIV_TOKEN_DEADLINE: %w", err)
	}

	// The library's token refresh uses http.DefaultClient, even with WithClient.
	http.DefaultClient.Transport = newPixivTransport(http.DefaultTransport.(*http.Transport))

	tokenDeadline := time.Now().Add(time.Duration(tokenDeadlineSec) * time.Second)
	_, err = pixiv.LoadAuth(
		os.Getenv("PIXIV_ACCESS_TOKEN"),
		os.Getenv("PIXIV_REFRESH_TOKEN"),
		tokenDeadline,
	)
	if err != nil {
		return nil, fmt.Errorf("logging into Pixiv: %w", err)
	}

	return pixiv.NewApp(), nil
}

type pixivTransport struct {
	base  *http.Transport
	pixiv *http.Transport
}

func newPixivTransport(base *http.Transport) *pixivTransport {
	transport := base.Clone()
	// Pixiv rejects Go's HTTP/2 requests with an HTML 403 instead of JSON.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &pixivTransport{base: base, pixiv: transport}
}

func (t *pixivTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Hostname() {
	case "oauth.secure.pixiv.net", "app-api.pixiv.net":
		return t.pixiv.RoundTrip(req)
	default:
		return t.base.RoundTrip(req)
	}
}
