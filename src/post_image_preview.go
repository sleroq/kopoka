package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
)

type postImagePreviewWork struct {
	variants map[string]*postImagePreviewVariant
	finished time.Time
}
type postImagePreviewVariant struct {
	done            chan struct{}
	data, thumbnail []byte
	width, height   int
	caption         string
	err             error
}

func postImagePreviewLink(link *url.URL) *url.URL {
	post := xPostLink(link)
	post.RawQuery, post.Fragment = "", ""
	return post
}

func (t *thumbnails) startPostImagePreview(link *url.URL) *postImagePreviewWork {
	link = postImagePreviewLink(link)
	key := link.String()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, work := range t.postImageWork {
		if !work.finished.IsZero() && time.Since(work.finished) >= 5*time.Minute {
			delete(t.postImageWork, k)
		}
	}
	if work := t.postImageWork[key]; work != nil {
		return work
	}
	work := &postImagePreviewWork{variants: map[string]*postImagePreviewVariant{
		"light": {done: make(chan struct{})}, "dark": {done: make(chan struct{})},
	}}
	t.postImageWork[key] = work
	go func() {
		t.workers <- struct{}{}
		defer func() { <-t.workers }()
		request, err := preparePostImageWithClient(t.client, link, "light")
		var wg sync.WaitGroup
		for theme, variant := range work.variants {
			wg.Add(1)
			go func(theme string, variant *postImagePreviewVariant) {
				defer wg.Done()
				variant.render(link, request, theme, err)
			}(theme, variant)
		}
		wg.Wait()
		t.mu.Lock()
		work.finished = time.Now()
		if work.variants["light"].err != nil || work.variants["dark"].err != nil {
			delete(t.postImageWork, key)
		}
		t.mu.Unlock()
	}()
	return work
}

func (variant *postImagePreviewVariant) render(link *url.URL, request renderRequest, theme string, prepareErr error) {
	defer close(variant.done)
	variant.err = prepareErr
	if prepareErr != nil {
		return
	}
	request.Theme = theme
	variant.caption = postImageCaption(link, request)
	variant.data, variant.err = renderPostImage(request)
	if variant.err != nil {
		return
	}
	img, err := jpeg.Decode(bytes.NewReader(variant.data))
	if err != nil {
		variant.err = err
		return
	}
	bounds := img.Bounds()
	variant.width, variant.height = bounds.Dx(), bounds.Dy()
	longest := max(variant.width, variant.height)
	width := max(1, int(math.Round(float64(variant.width)*384/float64(longest))))
	height := max(1, int(math.Round(float64(variant.height)*384/float64(longest))))
	small := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(small, small.Bounds(), img, bounds, draw.Src, nil)
	var out bytes.Buffer
	variant.err = jpeg.Encode(&out, small, &jpeg.Options{Quality: 90})
	variant.thumbnail = out.Bytes()
}

func (t *thumbnails) postImagePreview(link *url.URL, theme string, instant bool) previewPhoto {
	if instant {
		return t.postImagePlaceholder(theme)
	}
	link = postImagePreviewLink(link)
	variant := t.startPostImagePreview(link).variants[theme]
	select {
	case <-variant.done:
		if variant.err == nil {
			address := *t.base
			address.Path += "/post-image-preview"
			address.RawQuery = url.Values{"url": {link.String()}, "theme": {theme}, "v": {strconv.Itoa(postImageVersion)}}.Encode()
			u := address.String()
			return previewPhoto{photoURL: u + "&size=large", thumbnailURL: u, width: variant.width, height: variant.height}
		}
		log.Printf("generating post image preview for %s (%s): %v", link, theme, variant.err)
	case <-time.After(8 * time.Second):
	}
	return t.postImagePlaceholder(theme)
}

func (t *thumbnails) existingPostImagePreview(link *url.URL, theme string) *postImagePreviewVariant {
	t.mu.Lock()
	defer t.mu.Unlock()
	if work := t.postImageWork[postImagePreviewLink(link).String()]; work != nil {
		return work.variants[theme]
	}
	return nil
}

func (variant *postImagePreviewVariant) photo() (tgbotapi.InputMediaPhoto, error) {
	<-variant.done
	if variant.err != nil {
		return tgbotapi.InputMediaPhoto{}, variant.err
	}
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "post.jpg", Bytes: variant.data})
	photo.Caption, photo.ParseMode = variant.caption, "HTML"
	return photo, nil
}

func (t *thumbnails) servePostImagePreview(w http.ResponseWriter, r *http.Request) {
	for _, size := range r.URL.Query()["size"] {
		if size != "large" {
			http.Error(w, "invalid size", http.StatusBadRequest)
			return
		}
	}
	link := inlineLink(r.URL.Query().Get("url"))
	theme := r.URL.Query().Get("theme")
	if link == nil || !xLinkHosts[strings.ToLower(link.Host)] || (theme != "light" && theme != "dark") {
		http.Error(w, "invalid URL or theme", http.StatusBadRequest)
		return
	}
	variant := t.startPostImagePreview(link).variants[theme]
	select {
	case <-r.Context().Done():
		return
	case <-variant.done:
		if variant.err != nil {
			log.Printf("generating post image preview for %s (%s): %v", link, theme, variant.err)
			http.Error(w, "post image preview generation failed", http.StatusBadGateway)
			return
		}
		data := variant.thumbnail
		if r.URL.Query().Get("size") == "large" {
			data = variant.data
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}
}
