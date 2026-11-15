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
	forcedDone    chan struct{}
	forced        thumbnailVariant
	photoCount    int
	sourceOK      bool
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
	mergedPlaceholder []byte
	client            *http.Client
	mu                sync.Mutex
	work              map[string]*thumbnailWork
	postImageWork     map[string]*postImagePreviewWork
	workers           chan struct{}
	logos             map[bool]image.Image
	font              *opentype.Font
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
	postImages, err := renderPostImagePlaceholders(bold)
	if err != nil {
		return nil, err
	}
	return &thumbnails{
		base: base, app: app, bot: bot, db: db, cacheChat: cacheChat, placeholders: placeholders, postImages: postImages, mergedPlaceholder: mergedPlaceholder.Bytes(), client: &http.Client{Timeout: 15 * time.Second},
		work: make(map[string]*thumbnailWork), postImageWork: make(map[string]*postImagePreviewWork), workers: make(chan struct{}, 6),
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

func (t *thumbnails) savedPhoto(a artwork) previewPhoto {
	if strings.HasSuffix(a.ID, ":merged") && mediaType(a, 0) == "photo" {
		return previewPhoto{fileID: a.Photos[0]}
	}
	if link := inlineLink(a.URL); link != nil {
		count := len(a.Photos)
		if id := t.cached(link, count); id != "" {
			return previewPhoto{fileID: id}
		}
		t.warmArtwork(link, count)
		if a.PreviewWidth > 0 && a.PreviewHeight > 0 {
			return t.urlPhoto(link, count, a.PreviewWidth, a.PreviewHeight)
		}
	}
	if mediaType(a, 0) == "photo" {
		return previewPhoto{fileID: a.Photos[0]}
	}
	return t.placeholder(strings.HasPrefix(a.ID, "pixiv:"))
}

func (t *thumbnails) newPhoto(link *url.URL, instant bool) previewPhoto {
	if id := t.cached(link, 0); id != "" {
		return previewPhoto{fileID: id}
	}
	if !instant {
		work := t.warmup(link, 0)
		select {
		case <-work.done:
			if work.err == nil {
				return t.urlPhoto(link, 0, work.width, work.height)
			}
		case <-time.After(8 * time.Second):
		}
	}
	return t.placeholder(link.Host == "pixiv.net" || link.Host == "www.pixiv.net")
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

func (t *thumbnails) mergedPlaceholderPhoto() previewPhoto {
	address := *t.base
	address.Path += "/thumbnail-placeholder-merged.jpg"
	return previewPhoto{photoURL: address.String(), thumbnailURL: address.String(), width: 384, height: 384}
}

func (t *thumbnails) mergedPhoto(link *url.URL, instant bool) (previewPhoto, bool) {
	if xPhotoIndex(link) > 0 {
		return previewPhoto{}, false
	}

	var work *thumbnailWork
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
		return t.mergedPlaceholderPhoto(), true
	}
	work = t.warmup(link, 0)
	select {
	case <-work.forcedDone:
		if work.sourceOK && work.photoCount < 2 {
			return previewPhoto{}, false
		}
		if work.forced.err == nil {
			address := *t.base
			address.Path += "/thumbnail"
			address.RawQuery = url.Values{
				"url": {link.String()}, "v": {strconv.Itoa(thumbnailRenderVersion)}, "merge": {"1"},
			}.Encode()
			u := address.String()
			return previewPhoto{
				photoURL: u + "&size=large", thumbnailURL: u,
				width: work.forced.width, height: work.forced.height,
			}, true
		}
	case <-time.After(8 * time.Second):
	}
	return t.mergedPlaceholderPhoto(), true
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

	images := make(map[string][]byte, 2)
	for _, theme := range []string{"light", "dark"} {
		background, foreground, title := color.White, color.Black, "Light"
		if theme == "dark" {
			background, foreground, title = color.Black, color.White, "Dark"
		}
		canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
		drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(foreground), Face: face}
		drawer.Dot = fixed.P((384-drawer.MeasureString("Post image").Ceil())/2, 180)
		drawer.DrawString("Post image")
		drawer.Src = image.NewUniform(color.NRGBA{29, 155, 240, 255})
		drawer.Dot = fixed.P((384-drawer.MeasureString(title).Ceil())/2, 222)
		drawer.DrawString(title)
		var out bytes.Buffer
		if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
		images[theme] = out.Bytes()
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
				done: make(chan struct{}), forcedDone: make(chan struct{}),
				err: fmt.Errorf("thumbnail service busy"),
			}
			w.forced.err = w.err
			w.caching = make(map[int]bool)
			close(w.done)
			close(w.forcedDone)
			return w
		}
		delete(t.work, oldestKey)
	}

	w := &thumbnailWork{done: make(chan struct{}), forcedDone: make(chan struct{}), caching: make(map[int]bool)}
	t.work[key] = w
	go func() {
		t.workers <- struct{}{}
		source, err := t.source(link)
		w.sourceOK = err == nil
		var media []any
		if err == nil {
			media, err = t.downloadSource(source)
		}
		for _, item := range source.media {
			if item.Type == "" || item.Type == "photo" {
				w.photoCount++
			}
		}
		go func(sourceErr error) {
			defer close(w.forcedDone)
			w.forced.err = sourceErr
			if sourceErr == nil {
				w.forced.data, w.forced.large, _, w.forced.err = t.render(source, media, true)
				if w.forced.err == nil {
					var config image.Config
					config, w.forced.err = jpeg.DecodeConfig(bytes.NewReader(w.forced.large))
					w.forced.width, w.forced.height = config.Width, config.Height
				}
			}
		}(err)
		if err == nil {
			w.data, w.large, w.actual, err = t.render(source, media, false)
		}
		if err == nil {
			var config image.Config
			config, err = jpeg.DecodeConfig(bytes.NewReader(w.large))
			w.width, w.height = config.Width, config.Height
		}
		t.mu.Lock()
		w.err, w.finished = err, time.Now()
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
	if work.caching[count] {
		t.mu.Unlock()
		return
	}
	work.caching[count] = true
	t.mu.Unlock()
	go func() {
		<-work.done
		if work.err != nil {
			log.Printf("generating thumbnail for %s: %v", link, work.err)
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
	id, err := cacheInlineMedia(t.bot, t.cacheChat, photo)
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
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-merged.jpg"):
		placeholder = t.mergedPlaceholder
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-x.jpg"):
		placeholder = t.placeholders[false]
	case strings.HasSuffix(r.URL.Path, "/thumbnail-placeholder-pixiv.jpg"):
		placeholder = t.placeholders[true]
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-light.jpg"):
		placeholder = t.postImages["light"]
	case strings.HasSuffix(r.URL.Path, "/post-image-placeholder-dark.jpg"):
		placeholder = t.postImages["dark"]
	case strings.HasSuffix(r.URL.Path, "/post-image-preview"):
		t.servePostImagePreview(w, r)
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
	if r.URL.Query().Get("merge") == "1" {
		done = work.forcedDone
	}
	select {
	case <-r.Context().Done():
		return
	case <-done:
		var data, large []byte
		var err error
		if r.URL.Query().Get("merge") == "1" {
			data, large, err = work.forced.data, work.forced.large, work.forced.err
		} else {
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

type thumbnailSource struct {
	media      []thumbnailMedia
	count      int
	selected   int
	pixiv      bool
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
	source := thumbnailSource{pixiv: true, count: max(1, illust.PageCount, len(illust.MetaPages))}
	if illust.Type != "ugoira" && len(illust.MetaPages) > 1 {
		source.pixivPages = true
		for _, page := range illust.MetaPages {
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
	if res.StatusCode != http.StatusOK {
		return thumbnailSource{}, fmt.Errorf("FxTwitter HTTP %s", res.Status)
	}

	type fxMedia struct {
		All    []thumbnailMedia `json:"all"`
		Photos []thumbnailMedia `json:"photos"`
	}
	var response struct {
		Code  int `json:"code"`
		Tweet *struct {
			Media fxMedia `json:"media"`
			Quote struct {
				Media fxMedia `json:"media"`
			} `json:"quote"`
		} `json:"tweet"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return thumbnailSource{}, err
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return thumbnailSource{}, fmt.Errorf("FxTwitter API error %d", response.Code)
	}

	items := response.Tweet.Media.All
	if items == nil {
		items = response.Tweet.Media.Photos
	}
	if len(items) == 0 {
		items = response.Tweet.Quote.Media.All
		if items == nil {
			items = response.Tweet.Quote.Media.Photos
		}
	}
	if len(items) == 0 {
		return thumbnailSource{}, fmt.Errorf("FxTwitter preview missing")
	}
	count := len(items)
	selected := xPhotoIndex(link)
	if selected > 0 {
		if selected > count {
			return thumbnailSource{}, fmt.Errorf("FxTwitter photo %d out of range", selected)
		}
		item := items[selected-1]
		if item.Type != "" && item.Type != "photo" {
			return thumbnailSource{}, fmt.Errorf("FxTwitter media %d is not a photo", selected)
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
	return thumbnailSource{media: items, count: count, selected: selected}, nil
}

func (t *thumbnails) downloadSource(source thumbnailSource) ([]any, error) {
	media := make([]any, 0, len(source.media))
	for i, item := range source.media {
		if item.Type == "video" || item.Type == "gif" {
			if i > 0 {
				media = append(media, tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(item.ThumbnailURL)))
				continue
			}
			file, err := downloadArtFile(t.client, tgbotapi.FileURL(item.ThumbnailURL))
			if err != nil {
				return nil, err
			}
			media = append(media, tgbotapi.NewInputMediaVideo(file))
			continue
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
		media = append(media, tgbotapi.NewInputMediaPhoto(file))
	}
	return media, nil
}

func (t *thumbnails) mergeSource(source thumbnailSource, media []any, force bool) (image.Image, int, bool, error) {
	first, isPhoto := media[0].(tgbotapi.InputMediaPhoto)
	if force {
		var err error
		media, err = forceMergeArtPhotos(media)
		if err != nil {
			return nil, 0, false, err
		}
	} else {
		media = mergeArtPhotos(media)
	}
	merged := false
	if isPhoto {
		joined := media[0].(tgbotapi.InputMediaPhoto)
		merged = !bytes.Equal(first.Media.(tgbotapi.FileBytes).Bytes, joined.Media.(tgbotapi.FileBytes).Bytes)
	}
	actual := source.count
	if len(source.media) > 1 || source.selected > 0 {
		actual = len(media)
	}
	img, err := downloadArtPhoto(t.client, mediaBase(media[0]).Media)
	return img, actual, merged, err
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
	return t.render(source, media, false)
}

func (t *thumbnails) render(source thumbnailSource, media []any, force bool) ([]byte, []byte, int, error) {
	img, actual, merged, err := t.mergeSource(source, media, force)
	if err != nil {
		return nil, nil, 0, err
	}

	b := img.Bounds()
	longest := max(b.Dx(), b.Dy())
	if min(b.Dx(), b.Dy()) <= 0 {
		return nil, nil, 0, fmt.Errorf("empty preview image")
	}
	size := min(1280, max(384, longest))
	width := max(1, int(math.Round(float64(b.Dx())*float64(size)/float64(longest))))
	height := max(1, int(math.Round(float64(b.Dy())*float64(size)/float64(longest))))
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), img, b, draw.Over, nil)
	if err := t.decorate(canvas, source.pixiv, source.count, source.selected, merged); err != nil {
		return nil, nil, 0, err
	}

	var large bytes.Buffer
	if err := jpeg.Encode(&large, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, nil, 0, err
	}
	small := image.NewRGBA(image.Rect(0, 0,
		max(1, int(math.Round(float64(b.Dx())*384/float64(longest)))),
		max(1, int(math.Round(float64(b.Dy())*384/float64(longest))))))
	draw.CatmullRom.Scale(small, small.Bounds(), canvas, canvas.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, small, &jpeg.Options{Quality: 90}); err != nil {
		return nil, nil, 0, err
	}
	return out.Bytes(), large.Bytes(), actual, nil
}

func (t *thumbnails) decorate(canvas *image.RGBA, pix bool, count, selected int, merged bool) error {
	width, height := canvas.Bounds().Dx(), canvas.Bounds().Dy()
	// Offset shadows and a faint border keep overlays distinct against any artwork.
	tile := image.NewRGBA(image.Rect(0, 0, 76*4, 76*4))
	rounded(tile, image.Rect(2*4, 2*4, 76*4, 76*4), 15*4, color.NRGBA{0, 0, 0, 70})
	rounded(tile, image.Rect(0, 0, 74*4, 74*4), 15*4, color.NRGBA{255, 255, 255, 100})
	background := color.NRGBA{0, 0, 0, 255}
	if pix {
		background = color.NRGBA{0, 150, 250, 255}
	}
	rounded(tile, image.Rect(1*4, 1*4, 73*4, 73*4), 14*4, background)
	logo := t.logos[pix]
	at := image.Pt(37*4-logo.Bounds().Dx()/2, 37*4-logo.Bounds().Dy()/2)
	draw.Draw(tile, image.Rectangle{Min: at, Max: at.Add(logo.Bounds().Size())}, logo, logo.Bounds().Min, draw.Over)
	shorter := min(width, height)
	tileSize := max(1, int(math.Round(float64(shorter)*76/384)))
	margin := int(math.Round(float64(shorter) * 12 / 384))
	tileBounds := image.Rect(width-margin-tileSize, height-margin-tileSize, width-margin, height-margin)
	draw.CatmullRom.Scale(canvas, tileBounds, tile, tile.Bounds(), draw.Over, nil)

	if count <= 1 && selected == 0 && !merged {
		return nil
	}
	badge := image.NewRGBA(image.Rect(0, 0, 384*4, 56*4))
	right := 370 * 4
	if count > 1 || selected > 0 {
		label := strconv.Itoa(count)
		if selected > 0 {
			label = strconv.Itoa(selected) + "/" + label
		}
		left, err := t.badge(badge, label)
		if err != nil {
			return err
		}
		right = left - 8*4
	}
	if merged {
		left := right - 38*4
		badgeBackground(badge, left, right)
		for i := range 4 {
			x := left + (9+i*6)*4
			rounded(badge, image.Rect(x, 26*4, x+2*4, 42*4), 1*4, color.NRGBA{255, 255, 255, 255})
		}
	}
	badgeHeight := max(1, int(math.Round(float64(shorter)*56/384)))
	draw.CatmullRom.Scale(canvas, image.Rect(width-shorter, 0, width, badgeHeight), badge, badge.Bounds(), draw.Over, nil)
	return nil
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

func (t *thumbnails) badge(dst *image.RGBA, label string) (int, error) {
	face, err := opentype.NewFace(t.font, &opentype.FaceOptions{Size: 22 * 4, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return 0, err
	}
	defer func() { _ = face.Close() }()
	width := font.MeasureString(face, label).Ceil()
	left := 370*4 - (width + 47*4)
	badgeBackground(dst, left, 370*4)
	rounded(dst, image.Rect(left+12*4, 26*4, left+25*4, 37*4), 2*4, color.NRGBA{255, 255, 255, 150})
	rounded(dst, image.Rect(left+15*4, 29*4, left+28*4, 40*4), 2*4, color.NRGBA{255, 255, 255, 255})
	d := font.Drawer{Dst: dst, Src: image.NewUniform(color.White), Face: face, Dot: fixed.P(left+35*4, 42*4)}
	d.DrawString(label)
	return left, nil
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
