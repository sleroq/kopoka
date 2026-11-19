package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestThumbnailSourceDownloadsConcurrentlyInOrder(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		if active == 6 {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(3 * time.Second):
			t.Error("downloads did not run concurrently")
		}
		_, _ = fmt.Fprint(w, r.URL.Path)
		mu.Lock()
		active--
		mu.Unlock()
	}))
	defer server.Close()

	source := thumbnailSource{pixivPages: true}
	for i := range 6 {
		source.media = append(source.media, thumbnailMedia{ThumbnailURL: server.URL + "/" + strconv.Itoa(i)})
	}
	media, err := (&thumbnails{client: server.Client()}).downloadSource(source)
	if err != nil {
		t.Fatal(err)
	}
	if maximum != 6 {
		t.Fatalf("maximum concurrent downloads = %d, want 6", maximum)
	}
	for i, item := range media {
		photo := item.(tgbotapi.InputMediaPhoto)
		file := photo.Media.(tgbotapi.FileBytes)
		if string(file.Bytes) != "/"+strconv.Itoa(i) {
			t.Fatalf("image %d = %q", i, file.Bytes)
		}
	}
}
