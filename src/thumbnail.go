package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
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
	done     chan struct{}
	data     []byte
	actual   int
	err      error
	finished time.Time
	caching  bool
}

type thumbnails struct {
	base         *url.URL
	app          *pixiv.AppPixivAPI
	bot          *tgbotapi.BotAPI
	db           *sql.DB
	cacheChat    string
	placeholders map[bool][]byte
	client       *http.Client
	mu           sync.Mutex
	work         map[string]*thumbnailWork
	workers      chan struct{}
	logos        map[bool]image.Image
	font         *opentype.Font
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
	return &thumbnails{
		base: base, app: app, bot: bot, db: db, cacheChat: cacheChat, placeholders: placeholders, client: &http.Client{Timeout: 15 * time.Second},
		work: make(map[string]*thumbnailWork), workers: make(chan struct{}, 6),
		logos: logos, font: bold,
	}, nil
}

func (t *thumbnails) cached(link *url.URL, count int) string {
	id, err := findThumbnail(t.db, artworkID(link), count)
	if err != nil {
		log.Printf("loading cached thumbnail for %s: %v", link, err)
	}
	return id
}

func (t *thumbnails) photo(link *url.URL, count int) (fileID, photoURL string) {
	if id := t.cached(link, count); id != "" {
		return id, ""
	}
	return "", t.url(link, count)
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
	q := url.Values{"url": {link.String()}}
	if count > 0 {
		q.Set("count", strconv.Itoa(count))
	}
	result.RawQuery = q.Encode()
	work := t.start(link, count)
	t.cacheWork(link, count, work)
	return result.String()
}

func (t *thumbnails) start(link *url.URL, count int) *thumbnailWork {
	key := link.String() + "\n" + strconv.Itoa(count)
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
			w := &thumbnailWork{done: make(chan struct{}), err: fmt.Errorf("thumbnail service busy")}
			close(w.done)
			return w
		}
		delete(t.work, oldestKey)
	}

	w := &thumbnailWork{done: make(chan struct{})}
	t.work[key] = w
	go func() {
		t.workers <- struct{}{}
		data, actual, err := t.generate(link, count)
		t.mu.Lock()
		w.data, w.actual, w.err, w.finished = data, actual, err, time.Now()
		if err != nil {
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
	if work.caching {
		t.mu.Unlock()
		return
	}
	work.caching = true
	t.mu.Unlock()
	go func() {
		<-work.done
		if work.err != nil {
			return
		}
		t.workers <- struct{}{}
		if t.cached(link, count) == "" {
			t.cachePhoto(link, count, work.actual, work.data)
		}
		<-t.workers
	}()
}

func (t *thumbnails) cachePhoto(link *url.URL, count, actual int, data []byte) {
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "thumbnail.jpg", Bytes: data})
	id, err := cacheInlineMedia(t.bot, t.cacheChat, photo)
	if err != nil {
		log.Printf("uploading thumbnail for %s: %v", link, err)
		return
	}
	if err := saveThumbnail(t.db, artworkID(link), count, actual, id); err != nil {
		log.Printf("saving thumbnail for %s: %v", link, err)
	}
}

func (t *thumbnails) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var placeholder []byte
	switch {
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-x.jpg"):
		placeholder = t.placeholders[false]
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-pixiv.jpg"):
		placeholder = t.placeholders[true]
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
	link := inlineLink(r.URL.Query().Get("url"))
	if link == nil {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}
	count := 0
	if r.URL.Query().Has("count") {
		var err error
		count, err = strconv.Atoi(r.URL.Query().Get("count"))
		if err != nil || count <= 0 {
			http.Error(w, "invalid count", http.StatusBadRequest)
			return
		}
	}
	work := t.start(link, count)
	select {
	case <-r.Context().Done():
		return
	case <-work.done:
		data := work.data
		if work.err != nil {
			log.Printf("generating thumbnail for %s: %v", link, work.err)
			art, err := findArtwork(t.db, artworkID(link))
			if err != nil || art == nil {
				http.Error(w, "thumbnail generation failed", http.StatusBadGateway)
				return
			}
			pixiv := link.Host == "pixiv.net" || link.Host == "www.pixiv.net"
			data = t.placeholders[pixiv]
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

func (t *thumbnails) source(link *url.URL) (string, int, bool, error) {
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		preview, count, err := t.pixivSource(link)
		return preview, count, true, err
	}
	preview, count, err := t.xSource(link)
	return preview, count, false, err
}

func (t *thumbnails) pixivSource(link *url.URL) (string, int, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(link.Path, "/artworks/"), 10, 64)
	if err != nil {
		return "", 0, err
	}
	if t.app == nil {
		return "", 0, fmt.Errorf("missing Pixiv client")
	}
	pixivRequestMu.Lock()
	illust, err := t.app.IllustDetail(id)
	pixivRequestMu.Unlock()
	if err != nil {
		return "", 0, err
	}
	if illust == nil || illust.Images == nil {
		return "", 0, fmt.Errorf("missing Pixiv preview")
	}
	preview := illust.Images.Medium
	if preview == "" {
		preview = illust.Images.Large
	}
	if preview == "" {
		return "", 0, fmt.Errorf("missing Pixiv preview")
	}
	return preview, max(1, illust.PageCount, len(illust.MetaPages)), nil
}

func (t *thumbnails) xSource(link *url.URL) (string, int, error) {
	api := url.URL{Scheme: "https", Host: "api.fxtwitter.com", Path: link.Path}
	res, err := t.client.Get(api.String())
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("FxTwitter HTTP %s", res.Status)
	}

	var response struct {
		Code  int `json:"code"`
		Tweet *struct {
			Media struct {
				All    []thumbnailMedia `json:"all"`
				Photos []thumbnailMedia `json:"photos"`
			} `json:"media"`
		} `json:"tweet"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return "", 0, err
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return "", 0, fmt.Errorf("FxTwitter API error %d", response.Code)
	}

	items := response.Tweet.Media.All
	if items == nil {
		items = response.Tweet.Media.Photos
	}
	for _, item := range items {
		preview := item.URL
		if item.Type == "video" || item.Type == "gif" {
			preview = item.ThumbnailURL
		}
		if (item.Type == "" || item.Type == "photo" || item.Type == "video" || item.Type == "gif") && preview != "" {
			return preview, len(items), nil
		}
	}
	return "", 0, fmt.Errorf("FxTwitter preview missing")
}

func (t *thumbnails) generate(link *url.URL, count int) ([]byte, int, error) {
	source, actual, pix, err := t.source(link)
	if err != nil {
		return nil, 0, err
	}

	img, err := downloadArtPhoto(t.client, tgbotapi.FileURL(source))
	if err != nil {
		return nil, 0, err
	}
	if count == 0 {
		count = actual
	}

	b := img.Bounds()
	size := min(b.Dx(), b.Dy())
	if size <= 0 {
		return nil, 0, fmt.Errorf("empty preview image")
	}
	origin := image.Pt(b.Min.X+(b.Dx()-size)/2, b.Min.Y+(b.Dy()-size)/2)
	crop := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(size, size))}
	canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), img, crop, draw.Over, nil)
	// Offset shadows and a faint border keep overlays distinct against any artwork.
	rounded(canvas, image.Rect(298, 298, 372, 372), 15, color.NRGBA{0, 0, 0, 70})
	rounded(canvas, image.Rect(296, 296, 370, 370), 15, color.NRGBA{255, 255, 255, 100})
	background := color.NRGBA{0, 0, 0, 255}
	if pix {
		background = color.NRGBA{0, 150, 250, 255}
	}
	rounded(canvas, image.Rect(297, 297, 369, 369), 14, background)
	logo := t.logos[pix]
	at := image.Pt(333-logo.Bounds().Dx()/2, 333-logo.Bounds().Dy()/2)
	draw.Draw(canvas, image.Rectangle{Min: at, Max: at.Add(logo.Bounds().Size())}, logo, logo.Bounds().Min, draw.Over)
	if count > 1 {
		if err := t.badge(canvas, count); err != nil {
			return nil, 0, err
		}
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), actual, nil
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

func (t *thumbnails) badge(dst *image.RGBA, count int) error {
	face, err := opentype.NewFace(t.font, &opentype.FaceOptions{Size: 22, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer func() { _ = face.Close() }()
	label := strconv.Itoa(count)
	width := font.MeasureString(face, label).Ceil()
	left := 370 - (width + 47)
	rounded(dst, image.Rect(left+2, 17, 372, 55), 12, color.NRGBA{0, 0, 0, 70})
	rounded(dst, image.Rect(left, 15, 370, 53), 12, color.NRGBA{255, 255, 255, 100})
	rounded(dst, image.Rect(left+1, 16, 369, 52), 11, color.NRGBA{20, 22, 27, 235})
	rounded(dst, image.Rect(left+12, 26, left+25, 37), 2, color.NRGBA{255, 255, 255, 150})
	rounded(dst, image.Rect(left+15, 29, left+28, 40), 2, color.NRGBA{255, 255, 255, 255})
	d := font.Drawer{Dst: dst, Src: image.NewUniform(color.White), Face: face, Dot: fixed.P(left+35, 42)}
	d.DrawString(label)
	return nil
}
