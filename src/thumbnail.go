package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

//go:embed assets/x.png
var xLogo []byte

//go:embed assets/pixiv.png
var pixivLogo []byte

type thumbnailWork struct {
	done          chan struct{}
	sourceDone    chan struct{}
	forcedDone    chan struct{}
	barsDone      chan struct{}
	bars          thumbnailVariant
	forced        thumbnailVariant
	photoCount    int
	moving        bool
	sourceOK      bool
	sourceErr     error
	caching       map[int]bool
	data          []byte
	large         []byte
	actual        int
	width, height int
	err           error
	finished      time.Time
}

type thumbnailVariant struct {
	data, large   []byte
	width, height int
	err           error
}

type previewPhoto struct {
	fileID, photoURL, thumbnailURL string
	width, height                  int
}

type thumbnails struct {
	base              *url.URL
	app               *pixiv.AppPixivAPI
	bot               *tgbotapi.BotAPI
	db                *sql.DB
	cacheChat         string
	placeholders      map[bool][]byte
	postImages        map[string][]byte
	postTextIcon      []byte
	mergedPlaceholder []byte
	barsPlaceholder   []byte
	client            *http.Client
	mu                sync.Mutex
	themePreviewMu    sync.Mutex
	themePreviews     map[string]themePreviewImages
	work              map[string]*thumbnailWork
	postImageWork     map[string]*postImagePreviewWork
	postTextWork      map[string]*postTextPreviewWork
	workers           chan struct{}
}

func startThumbnails(app *pixiv.AppPixivAPI, bot *tgbotapi.BotAPI, db *sql.DB, cacheChat string) (*thumbnails, error) {
	baseURL := os.Getenv("THUMBNAIL_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("THUMBNAIL_BASE_URL is required")
	}
	previews, err := newThumbnails(baseURL, app, bot, db, cacheChat)
	if err != nil {
		return nil, err
	}
	address := os.Getenv("THUMBNAIL_LISTEN")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: previews, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil {
			log.Printf("thumbnail server: %v", err)
		}
	}()
	return previews, nil
}

func thumbnailBaseURL(baseURL string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid thumbnail base URL")
	}
	host := base.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if base.Scheme != "https" && (base.Scheme != "http" || !loopback) {
		return nil, fmt.Errorf("thumbnail base URL must use HTTPS and a public host")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	return base, nil
}

func newThumbnails(baseURL string, app *pixiv.AppPixivAPI, bot *tgbotapi.BotAPI, db *sql.DB, cacheChat string) (*thumbnails, error) {
	base, err := thumbnailBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	x, err := png.Decode(bytes.NewReader(xLogo))
	if err != nil {
		return nil, err
	}
	pix, err := png.Decode(bytes.NewReader(pixivLogo))
	if err != nil {
		return nil, err
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, err
	}
	logos := map[bool]image.Image{false: x, true: pix}
	placeholders := make(map[bool][]byte, len(logos))
	for isPixiv, logo := range logos {
		background := color.NRGBA{20, 22, 27, 255}
		if isPixiv {
			background = color.NRGBA{0, 150, 250, 255}
		}
		canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
		draw.CatmullRom.Scale(canvas, image.Rect(112, 112, 272, 272), logo, logo.Bounds(), draw.Over, nil)
		var out bytes.Buffer
		if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
		placeholders[isPixiv] = out.Bytes()
	}
	canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{20, 22, 27, 255}), image.Point{}, draw.Src)
	for i := range 4 {
		x := 102 + i*47
		rounded(canvas, image.Rect(x, 120, x+30, 264), 8, color.NRGBA{255, 255, 255, 255})
	}
	var mergedPlaceholder bytes.Buffer
	if err := jpeg.Encode(&mergedPlaceholder, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{20, 22, 27, 255}), image.Point{}, draw.Src)
	for i := range 8 {
		x := 72 + i*31
		rounded(canvas, image.Rect(x, 120, x+23, 264), 8, color.NRGBA{255, 255, 255, 255})
	}
	var barsPlaceholder bytes.Buffer
	if err := jpeg.Encode(&barsPlaceholder, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	postImages, err := renderPostImagePlaceholders(bold)
	if err != nil {
		return nil, err
	}
	postTextIcon, err := renderPostTextIcon()
	if err != nil {
		return nil, err
	}
	return &thumbnails{
		base: base, app: app, bot: bot, db: db, cacheChat: cacheChat, placeholders: placeholders, postImages: postImages, postTextIcon: postTextIcon, mergedPlaceholder: mergedPlaceholder.Bytes(), barsPlaceholder: barsPlaceholder.Bytes(), client: &http.Client{Timeout: 15 * time.Second},
		work: make(map[string]*thumbnailWork), postImageWork: make(map[string]*postImagePreviewWork), postTextWork: make(map[string]*postTextPreviewWork), themePreviews: make(map[string]themePreviewImages), workers: make(chan struct{}, 6),
	}, nil
}

func (t *thumbnails) cached(link *url.URL, count int) string {
	id, err := findThumbnail(t.db, artworkID(link), count)
	if err != nil {
		log.Printf("loading cached thumbnail for %s: %v", link, err)
	}
	return id
}

func (t *thumbnails) savedPhoto(a artwork) previewPhoto {
	if link := inlineLink(a.URL); link != nil {
		bars := strings.HasSuffix(a.ID, ":merged:bars")
		if bars || strings.HasSuffix(a.ID, ":merged") {
			work := t.warmup(link, 0)
			done := work.forcedDone
			if bars {
				done = work.barsDone
			}
			select {
			case <-done:
				if photo, ok := t.mergedPhotoVariant(link, false, bars); ok {
					return photo
				}
			default:
			}
			return t.mergedPlaceholderPhoto(bars)
		}
		count := len(a.Photos)
		if id := t.cached(link, count); id != "" {
			return previewPhoto{fileID: id}
		}
		t.warmArtwork(link, count)
	}
	return t.placeholder(strings.HasPrefix(a.ID, "pixiv:"))
}

var errNoXMedia = errors.New("FxTwitter post has no media")
var errMergedPhotoDimensions = errors.New("merged photo dimensions exceed Telegram limits")

func (t *thumbnails) newPhoto(link *url.URL, instant bool) (previewPhoto, bool) {
	if id := t.cached(link, 0); id != "" {
		return previewPhoto{fileID: id}, true
	}
	if !instant {
		work := t.warmup(link, 0)
		select {
		case <-work.done:
			if errors.Is(work.err, errNoXMedia) {
				return previewPhoto{}, false
			}
			if work.err == nil {
				return t.urlPhoto(link, 0, work.width, work.height), true
			}
		case <-time.After(8 * time.Second):
		}
	}
	return t.placeholder(link.Host == "pixiv.net" || link.Host == "www.pixiv.net"), true
}

func (t *thumbnails) urlPhoto(link *url.URL, count, width, height int) previewPhoto {
	thumbnail := t.url(link, count)
	return previewPhoto{
		photoURL: thumbnail + "&size=large", thumbnailURL: thumbnail,
		width: width, height: height,
	}
}

func (t *thumbnails) placeholder(pixiv bool) previewPhoto {
	address := t.placeholderURL(pixiv)
	return previewPhoto{photoURL: address, thumbnailURL: address, width: 384, height: 384}
}

func (t *thumbnails) mergedPlaceholderPhoto(bars bool) previewPhoto {
	address := *t.base
	if bars {
		address.Path += "/thumbnail-placeholder-merged-bars.jpg"
	} else {
		address.Path += "/thumbnail-placeholder-merged.jpg"
	}
	return previewPhoto{photoURL: address.String(), thumbnailURL: address.String(), width: 384, height: 384}
}

func (t *thumbnails) mergedPhoto(link *url.URL, instant bool) (previewPhoto, bool) {
	return t.mergedPhotoVariant(link, instant, false)
}

func (t *thumbnails) mergedPhotoVariant(link *url.URL, instant, bars bool) (previewPhoto, bool) {
	if _, index := xMediaSelection(link); index > 0 || pixivPhotoIndex(link) > 0 {
		return previewPhoto{}, false
	}

	var work *thumbnailWork
	if !bars && xLinkHosts[strings.ToLower(link.Host)] {
		work = t.warmup(link, 0)
		select {
		case <-work.sourceDone:
			if !work.sourceOK || work.moving || work.photoCount < 2 {
				return previewPhoto{}, false
			}
		case <-time.After(8 * time.Second):
			return previewPhoto{}, false
		}
	}
	if instant {
		t.mu.Lock()
		work = t.work[link.String()]
		t.mu.Unlock()
		if work != nil {
			select {
			case <-work.done:
				if work.sourceOK && work.photoCount < 2 {
					return previewPhoto{}, false
				}
			case <-work.forcedDone:
				if work.sourceOK && work.photoCount < 2 {
					return previewPhoto{}, false
				}
			default:
			}
		}
		return t.mergedPlaceholderPhoto(bars), true
	}
	work = t.warmup(link, 0)
	done, variant, mode := work.forcedDone, &work.forced, "1"
	if bars {
		done, variant, mode = work.barsDone, &work.bars, "bars"
	}
	select {
	case <-done:
		if errors.Is(variant.err, errNoXMedia) || errors.Is(variant.err, errMergedPhotoDimensions) || work.sourceOK && work.photoCount < 2 {
			return previewPhoto{}, false
		}
		if variant.err == nil {
			address := *t.base
			address.Path += "/thumbnail"
			address.RawQuery = url.Values{
				"url": {link.String()}, "v": {strconv.Itoa(thumbnailRenderVersion)}, "merge": {mode},
			}.Encode()
			u := address.String()
			return previewPhoto{
				photoURL: u + "&size=large", thumbnailURL: u,
				width: variant.width, height: variant.height,
			}, true
		}
	case <-time.After(8 * time.Second):
	}
	return previewPhoto{}, false
}

func (t *thumbnails) postTextPhoto() previewPhoto {
	address := *t.base
	address.Path += "/post-text-placeholder.jpg"
	return previewPhoto{photoURL: address.String(), thumbnailURL: address.String(), width: 384, height: 384}
}

func renderPostTextIcon() ([]byte, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{20, 22, 27, 255}), image.Point{}, draw.Src)
	border := color.NRGBA{84, 96, 110, 255}
	card := color.NRGBA{31, 36, 44, 255}
	text := color.NRGBA{231, 233, 234, 255}
	muted := color.NRGBA{139, 152, 165, 255}
	accent := color.NRGBA{29, 155, 240, 255}

	rounded(canvas, image.Rect(57, 48, 327, 336), 24, border)
	rounded(canvas, image.Rect(59, 50, 325, 334), 22, card)
	rounded(canvas, image.Rect(83, 77, 121, 115), 19, accent)
	rounded(canvas, image.Rect(135, 81, 254, 91), 5, text)
	rounded(canvas, image.Rect(135, 101, 204, 109), 4, muted)
	for i, right := range []int{274, 258, 220} {
		y := 139 + i*21
		rounded(canvas, image.Rect(83, y, right, y+9), 4, text)
	}
	rounded(canvas, image.Rect(81, 213, 303, 310), 14, border)
	rounded(canvas, image.Rect(83, 215, 301, 308), 12, card)
	rounded(canvas, image.Rect(98, 230, 105, 290), 3, accent)
	for i, right := range []int{257, 270, 221} {
		y := 232 + i*23
		rounded(canvas, image.Rect(118, y, right, y+9), 4, muted)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (t *thumbnails) postImagePlaceholder(theme string) previewPhoto {
	address := *t.base
	address.Path += "/post-image-placeholder-" + theme + ".jpg"
	return previewPhoto{photoURL: address.String(), thumbnailURL: address.String(), width: 384, height: 384}
}

func renderPostImagePlaceholders(bold *opentype.Font) (map[string][]byte, error) {
	face, err := opentype.NewFace(bold, &opentype.FaceOptions{Size: 28, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer func() { _ = face.Close() }()

	images := make(map[string][]byte, 4)
	for _, theme := range []struct {
		id, title              string
		background, foreground color.Color
	}{
		{"light", "Light", color.White, color.Black},
		{"warm", "Warm", color.NRGBA{255, 246, 245, 255}, color.Black},
		{"dark", "Dark", color.Black, color.White},
		{"dim", "Dim", color.NRGBA{22, 24, 28, 255}, color.White},
	} {
		canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(theme.background), image.Point{}, draw.Src)
		drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(theme.foreground), Face: face}
		drawer.Dot = fixed.P((384-drawer.MeasureString("Post image").Ceil())/2, 180)
		drawer.DrawString("Post image")
		drawer.Src = image.NewUniform(color.NRGBA{29, 155, 240, 255})
		drawer.Dot = fixed.P((384-drawer.MeasureString(theme.title).Ceil())/2, 222)
		drawer.DrawString(theme.title)
		var out bytes.Buffer
		if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
		images[theme.id] = out.Bytes()
	}
	return images, nil
}

func (t *thumbnails) placeholderURL(pixiv bool) string {
	result := *t.base
	result.Path += "/thumbnail-placeholder-x.jpg"
	if pixiv {
		result.Path = t.base.Path + "/thumbnail-placeholder-pixiv.jpg"
	}
	return result.String()
}

func (t *thumbnails) url(link *url.URL, count int) string {
	result := *t.base
	result.Path += "/thumbnail"
	q := url.Values{"url": {link.String()}, "v": {strconv.Itoa(thumbnailRenderVersion)}}
	if count > 0 {
		q.Set("count", strconv.Itoa(count))
	}
	result.RawQuery = q.Encode()
	work := t.start(link)
	t.cacheWork(link, count, work)
	return result.String()
}

func (t *thumbnails) start(link *url.URL) *thumbnailWork {
	key := link.String()
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for k, w := range t.work {
		if !w.finished.IsZero() && now.Sub(w.finished) >= 5*time.Minute {
			delete(t.work, k)
		}
	}

	if w := t.work[key]; w != nil {
		return w
	}
	if len(t.work) >= 128 {
		var oldestKey string
		var oldest time.Time
		for k, w := range t.work {
			if !w.finished.IsZero() && (oldest.IsZero() || w.finished.Before(oldest)) {
				oldestKey, oldest = k, w.finished
			}
		}
		if oldestKey == "" {
			w := &thumbnailWork{
				done: make(chan struct{}), sourceDone: make(chan struct{}), forcedDone: make(chan struct{}), barsDone: make(chan struct{}),
				err: fmt.Errorf("thumbnail service busy"),
			}
			w.sourceErr = w.err
			w.forced.err = w.err
			w.bars.err = w.err
			w.caching = make(map[int]bool)
			close(w.done)
			close(w.sourceDone)
			close(w.forcedDone)
			close(w.barsDone)
			return w
		}
		delete(t.work, oldestKey)
	}

	w := &thumbnailWork{
		done: make(chan struct{}), sourceDone: make(chan struct{}), forcedDone: make(chan struct{}), barsDone: make(chan struct{}),
		caching: make(map[int]bool),
	}
	t.work[key] = w
	go func() {
		t.workers <- struct{}{}
		source, err := t.source(link)
		w.sourceOK = err == nil
		w.sourceErr = err
		w.photoCount = len(source.media)
		for _, item := range source.media {
			w.moving = w.moving || item.moving()
		}
		close(w.sourceDone)
		var media []any
		if err == nil {
			media, err = t.downloadSource(source)
		}
		sourceErr := err
		for _, item := range []struct {
			done    chan struct{}
			variant *thumbnailVariant
			mode    string
		}{
			{w.forcedDone, &w.forced, "1"}, {w.barsDone, &w.bars, "bars"},
		} {
			go func() {
				defer close(item.done)
				item.variant.err = sourceErr
				if sourceErr == nil {
					item.variant.data, item.variant.large, _, item.variant.err = t.render(source, media, item.mode)
					if item.variant.err == nil {
						var config image.Config
						config, item.variant.err = jpeg.DecodeConfig(bytes.NewReader(item.variant.large))
						item.variant.width, item.variant.height = config.Width, config.Height
					}
				}
			}()
		}
		if err == nil {
			w.data, w.large, w.actual, err = t.render(source, media, "")
		}
		if err == nil {
			var config image.Config
			config, err = jpeg.DecodeConfig(bytes.NewReader(w.large))
			w.width, w.height = config.Width, config.Height
		}
		t.mu.Lock()
		w.err, w.finished = err, time.Now()
		// A text-only post is a valid result. Only failed work needs a retry.
		if err != nil && !errors.Is(err, errNoXMedia) {
			delete(t.work, key)
		}
		close(w.done)
		t.mu.Unlock()
		<-t.workers
	}()
	return w
}

func (t *thumbnails) cacheWork(link *url.URL, count int, work *thumbnailWork) {
	t.mu.Lock()
	if work.caching[count] {
		t.mu.Unlock()
		return
	}
	work.caching[count] = true
	t.mu.Unlock()
	go func() {
		<-work.done
		if work.err != nil {
			if !errors.Is(work.err, errNoXMedia) {
				log.Printf("generating thumbnail for %s: %v", link, work.err)
			}
			return
		}
		t.workers <- struct{}{}
		if t.cached(link, count) == "" {
			if err := t.cachePhoto(link, count, work.actual, work.large); err != nil {
				log.Printf("caching thumbnail for %s: %v", link, err)
				t.mu.Lock()
				work.caching[count] = false
				t.mu.Unlock()
			}
		}
		<-t.workers
	}()
}

func (t *thumbnails) cachePhoto(link *url.URL, count, actual int, data []byte) error {
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "thumbnail.jpg", Bytes: data})
	_, id, err := cacheInlineMedia(t.bot, t.cacheChat, photo)
	if err != nil {
		return fmt.Errorf("uploading: %w", err)
	}
	if err := saveThumbnail(t.db, artworkID(link), count, actual, id); err != nil {
		return fmt.Errorf("saving: %w", err)
	}
	return nil
}

func (t *thumbnails) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var placeholder []byte
	switch {
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-merged-bars.jpg"):
		placeholder = t.barsPlaceholder
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-merged.jpg"):
		placeholder = t.mergedPlaceholder
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-x.jpg"):
		placeholder = t.placeholders[false]
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-pixiv.jpg"):
		placeholder = t.placeholders[true]
	case strings.HasSuffix(r.URL.Path, "/post-text-placeholder.jpg"):
		placeholder = t.postTextIcon
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-light.jpg"):
		placeholder = t.postImages["light"]
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-dark.jpg"):
		placeholder = t.postImages["dark"]
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-warm.jpg"):
		placeholder = t.postImages["warm"]
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-dim.jpg"):
		placeholder = t.postImages["dim"]
	case strings.HasSuffix(r.URL.Path, "/theme-preview"):
		t.serveThemePreview(w, r)
		return
	case strings.HasSuffix(r.URL.Path, "/post-image-preview"):
		t.servePostImagePreview(w, r)
		return
	case strings.HasSuffix(r.URL.Path, "/post-text-preview"):
		t.servePostTextPreview(w, r)
		return
	case strings.HasSuffix(r.URL.Path, "/thumbnail"):
		t.serveThumbnail(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(placeholder)
}

func (t *thumbnails) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	for _, size := range r.URL.Query()["size"] {
		if size != "large" {
			http.Error(w, "invalid size", http.StatusBadRequest)
			return
		}
	}

	link := inlineLink(r.URL.Query().Get("url"))
	if link == nil {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Has("count") {
		count, err := strconv.Atoi(r.URL.Query().Get("count"))
		if err != nil || count <= 0 {
			http.Error(w, "invalid count", http.StatusBadRequest)
			return
		}
	}
	work := t.start(link)
	done := work.done
	switch r.URL.Query().Get("merge") {
	case "1":
		done = work.forcedDone
	case "bars":
		done = work.barsDone
	}
	select {
	case <-r.Context().Done():
		return
	case <-done:
		var data, large []byte
		var err error
		switch r.URL.Query().Get("merge") {
		case "1":
			data, large, err = work.forced.data, work.forced.large, work.forced.err
		case "bars":
			data, large, err = work.bars.data, work.bars.large, work.bars.err
		default:
			data, large, err = work.data, work.large, work.err
		}
		if r.URL.Query().Get("size") == "large" {
			data = large
		}
		if err != nil {
			log.Printf("generating thumbnail for %s: %v", link, err)
			http.Error(w, "thumbnail generation failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}
}

type thumbnailMedia struct {
	Type         string `json:"type"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

func (media thumbnailMedia) moving() bool {
	return media.Type == "video" || media.Type == "gif"
}

type thumbnailSource struct {
	author     string
	video      bool
	media      []thumbnailMedia
	pixivPages bool
}

func (t *thumbnails) source(link *url.URL) (thumbnailSource, error) {
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		return t.pixivSource(link)
	}
	return t.xSource(link)
}

func (t *thumbnails) pixivSource(link *url.URL) (thumbnailSource, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(link.Path, "/artworks/"), 10, 64)
	if err != nil {
		return thumbnailSource{}, err
	}
	if t.app == nil {
		return thumbnailSource{}, fmt.Errorf("missing Pixiv client")
	}

	pixivRequestMu.Lock()
	illust, err := t.app.IllustDetail(id)
	pixivRequestMu.Unlock()
	if err != nil {
		return thumbnailSource{}, err
	}
	if illust == nil {
		return thumbnailSource{}, fmt.Errorf("missing Pixiv preview")
	}
	selected, err := selectPixivPhoto(illust, link)
	if err != nil {
		return thumbnailSource{}, err
	}

	source := thumbnailSource{video: illust.Type == "ugoira"}
	if illust.User != nil {
		source.author = illust.User.Name
	}
	if illust.Type != "ugoira" && len(illust.MetaPages) > 1 {
		source.pixivPages = true
		for i, page := range illust.MetaPages {
			if selected > 0 && i != selected-1 {
				continue
			}
			source.media = append(source.media, thumbnailMedia{URL: page.Images.Original, ThumbnailURL: page.Images.Large})
		}
		return source, nil
	}
	if illust.Images == nil {
		return thumbnailSource{}, fmt.Errorf("missing Pixiv preview")
	}
	preview := illust.Images.Large
	if preview == "" {
		preview = illust.Images.Medium
	}
	if preview == "" {
		return thumbnailSource{}, fmt.Errorf("missing Pixiv preview")
	}
	source.media = []thumbnailMedia{{URL: preview}}
	return source, nil
}

func (t *thumbnails) xSource(link *url.URL) (thumbnailSource, error) {
	api := url.URL{Scheme: "https", Host: "api.fxtwitter.com", Path: xPostLink(link).Path}
	res, err := t.client.Get(api.String())
	if err != nil {
		return thumbnailSource{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return thumbnailSource{}, errXPostNotFound
	}
	if res.StatusCode != http.StatusOK {
		return thumbnailSource{}, fmt.Errorf("FxTwitter HTTP %s", res.Status)
	}

	type fxMedia struct {
		All    []thumbnailMedia `json:"all"`
		Photos []thumbnailMedia `json:"photos"`
		Videos []thumbnailMedia `json:"videos"`
	}
	var response struct {
		Code  int `json:"code"`
		Tweet *struct {
			Media  fxMedia `json:"media"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Quote struct {
				Media fxMedia `json:"media"`
			} `json:"quote"`
		} `json:"tweet"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return thumbnailSource{}, err
	}
	if response.Code == http.StatusNotFound {
		return thumbnailSource{}, errXPostNotFound
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return thumbnailSource{}, fmt.Errorf("FxTwitter API error %d", response.Code)
	}

	for _, videos := range [][]thumbnailMedia{response.Tweet.Media.Videos, response.Tweet.Quote.Media.Videos} {
		for i := range videos {
			if videos[i].Type == "" {
				videos[i].Type = "video"
			}
		}
	}
	items := response.Tweet.Media.All
	if len(items) == 0 {
		items = append(items, response.Tweet.Media.Photos...)
		items = append(items, response.Tweet.Media.Videos...)
	}
	if len(items) == 0 {
		items = response.Tweet.Quote.Media.All
		if len(items) == 0 {
			items = append(items, response.Tweet.Quote.Media.Photos...)
			items = append(items, response.Tweet.Quote.Media.Videos...)
		}
	}
	if len(items) == 0 {
		return thumbnailSource{}, errNoXMedia
	}
	count := len(items)
	kind, selected := xMediaSelection(link)
	if selected > 0 {
		if selected > count {
			return thumbnailSource{}, fmt.Errorf("FxTwitter photo %d out of range", selected)
		}
		item := items[selected-1]
		if !xMediaMatches(kind, item.Type) {
			return thumbnailSource{}, fmt.Errorf("FxTwitter media %d does not match %s", selected, kind)
		}
		items = items[selected-1 : selected]
	}
	for i, item := range items {
		switch item.Type {
		case "video", "gif":
			if i == 0 && item.ThumbnailURL == "" {
				return thumbnailSource{}, fmt.Errorf("FxTwitter preview missing")
			}
		case "", "photo":
			if item.URL == "" {
				return thumbnailSource{}, fmt.Errorf("FxTwitter preview missing")
			}
		default:
			return thumbnailSource{}, fmt.Errorf("unsupported FxTwitter media type %q", item.Type)
		}
	}
	return thumbnailSource{media: items, author: response.Tweet.Author.Name}, nil
}

func (t *thumbnails) downloadSource(source thumbnailSource) ([]any, error) {
	media := make([]any, len(source.media))
	err := downloadGallery(len(source.media), func(i int) error {
		item, err := t.downloadSourceItem(source, i)
		media[i] = item
		return err
	}, nil)
	if err != nil {
		return nil, err
	}
	return media, nil
}

func (t *thumbnails) downloadSourceItem(source thumbnailSource, i int) (any, error) {
	item := source.media[i]
	if item.moving() {
		if i > 0 {
			return tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(item.ThumbnailURL)), nil
		}
		file, err := downloadArtFile(t.client, tgbotapi.FileURL(item.ThumbnailURL))
		if err != nil {
			return nil, err
		}
		return tgbotapi.NewInputMediaVideo(file), nil
	}
	var file tgbotapi.FileBytes
	var err error
	if source.pixivPages {
		file, err = downloadPixivPhoto(t.client, item.ThumbnailURL, item.URL)
	} else {
		file, err = downloadArtFile(t.client, tgbotapi.FileURL(xPhotoURL(item.URL)))
	}
	if err != nil {
		return nil, err
	}
	return tgbotapi.NewInputMediaPhoto(file), nil
}

func (t *thumbnails) mergeSource(media []any, mode string) (image.Image, int, error) {
	if mode != "" {
		media = append([]any(nil), media...)
		for i, item := range media {
			if video, ok := item.(tgbotapi.InputMediaVideo); ok {
				media[i] = tgbotapi.NewInputMediaPhoto(video.Media)
			}
		}
	}
	if mode != "" {
		var err error
		media, err = forceMergePanels(media, mode == "bars")
		if err != nil {
			return nil, 0, err
		}
	} else {
		media = mergeArtPhotos(media)
	}
	img, err := downloadArtPhoto(t.client, mediaBase(media[0]).Media)
	return img, len(media), err
}

func (t *thumbnails) generate(link *url.URL) ([]byte, []byte, int, error) {
	source, err := t.source(link)
	if err != nil {
		return nil, nil, 0, err
	}
	media, err := t.downloadSource(source)
	if err != nil {
		return nil, nil, 0, err
	}
	return t.render(source, media, "")
}

func (t *thumbnails) render(source thumbnailSource, media []any, mode string) ([]byte, []byte, int, error) {
	img, actual, err := t.mergeSource(media, mode)
	if err != nil {
		return nil, nil, 0, err
	}

	b := img.Bounds()
	if mode != "" && (b.Dx()+b.Dy() > 10000 || b.Dx() > 20*b.Dy() || b.Dy() > 20*b.Dx()) {
		return nil, nil, 0, errMergedPhotoDimensions
	}
	longest := max(b.Dx(), b.Dy())
	if min(b.Dx(), b.Dy()) <= 0 {
		return nil, nil, 0, fmt.Errorf("empty preview image")
	}
	size := min(1280, max(384, longest))
	width := max(1, int(math.Round(float64(b.Dx())*float64(size)/float64(longest))))
	height := max(1, int(math.Round(float64(b.Dy())*float64(size)/float64(longest))))
	canvasWidth := max(width, (height+19)/20)
	canvasHeight := max(height, (width+19)/20)
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	x, y := (canvasWidth-width)/2, (canvasHeight-height)/2
	draw.CatmullRom.Scale(canvas, image.Rect(x, y, x+width, y+height), img, b, draw.Over, nil)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, nil, 0, err
	}
	video := source.video || source.media[0].moving()
	count := actual
	if mode != "" {
		count = 1
		for _, item := range source.media {
			video = video || item.moving()
		}
	}
	blocks := []previewBlock{{Kind: "media", Image: encoded.Bytes(), Video: video, Count: count}}
	if source.author != "" {
		blocks = append(blocks, previewBlock{Kind: "caption", Text: source.author})
	}
	small, large, _, _, err := t.renderMessagePreview(blocks)
	return small, large, actual, err
}

func rounded(dst *image.RGBA, r image.Rectangle, radius float32, c color.NRGBA) {
	x0, y0, x1, y1 := float32(r.Min.X), float32(r.Min.Y), float32(r.Max.X), float32(r.Max.Y)
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	z.MoveTo(x0+radius, y0)
	z.LineTo(x1-radius, y0)
	z.QuadTo(x1, y0, x1, y0+radius)
	z.LineTo(x1, y1-radius)
	z.QuadTo(x1, y1, x1-radius, y1)
	z.LineTo(x0+radius, y1)
	z.QuadTo(x0, y1, x0, y1-radius)
	z.LineTo(x0, y0+radius)
	z.QuadTo(x0, y0, x0+radius, y0)
	z.ClosePath()
	z.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{})
}

func badgeBackground(dst *image.RGBA, left, right int) {
	rounded(dst, image.Rect(left+2*4, 17*4, right+2*4, 55*4), 12*4, color.NRGBA{0, 0, 0, 70})
	rounded(dst, image.Rect(left, 15*4, right, 53*4), 12*4, color.NRGBA{255, 255, 255, 100})
	rounded(dst, image.Rect(left+1*4, 16*4, right-1*4, 52*4), 11*4, color.NRGBA{20, 22, 27, 235})
}

func (t *thumbnails) warmup(link *url.URL, count int) *thumbnailWork {
	work := t.start(link)
	t.cacheWork(link, count, work)
	return work
}

func (t *thumbnails) saveGeneratedDimensions(link *url.URL, work *thumbnailWork) error {
	<-work.done
	if work.err != nil {
		return work.err
	}
	return savePreviewDimensions(t.db, artworkID(link), work.width, work.height)
}

func (t *thumbnails) warmArtwork(link *url.URL, count int) {
	go func() {
		t.mu.Lock()
		work := t.work[link.String()]
		t.mu.Unlock()
		if work == nil {
			work = t.warmup(link, count)
		} else {
			t.cacheWork(link, 0, work)
		}
		if err := t.saveGeneratedDimensions(link, work); err != nil {
			log.Printf("warming preview for %s: %v", link, err)
		}
	}()
}
