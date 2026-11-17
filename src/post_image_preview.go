package main

import (
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type postImagePreviewWork struct {
	variants   map[string]*postImagePreviewVariant
	prepared   chan struct{}
	request    renderRequest
	bars       renderRequest
	prepareErr error
	barsKnown  bool
	hasBars    bool
	active     int
	failed     bool
	finished   time.Time
}
type postImagePreviewVariant struct {
	request                  renderRequest
	done                     chan struct{}
	data, preview, thumbnail []byte
	width, height            int
	caption                  string
	err                      error
}

func postImagePreviewLink(link *url.URL) *url.URL {
	post := xPostLink(link)
	post.RawQuery, post.Fragment = "", ""
	return post
}

func (t *thumbnails) startPostImagePreview(link *url.URL, zone string, translation translationSettings, themes ...string) *postImagePreviewWork {
	link = postImagePreviewLink(link)
	key := artworkID(xPostLink(link)) + ":" + zone + ":" + translation.key()

	t.mu.Lock()
	defer t.mu.Unlock()
	for k, work := range t.postImageWork {
		if !work.finished.IsZero() && time.Since(work.finished) >= 5*time.Minute {
			delete(t.postImageWork, k)
		}
	}
	work := t.postImageWork[key]
	if work == nil {
		work = &postImagePreviewWork{prepared: make(chan struct{}), variants: make(map[string]*postImagePreviewVariant)}
		t.postImageWork[key] = work
		go func() {
			t.workers <- struct{}{}
			work.request, work.bars, work.prepareErr = preparePostImageVariants(t.client, link, zone, translation, true)
			work.barsKnown = work.prepareErr == nil
			work.hasBars = work.bars.Post.Media.hasBars()
			close(work.prepared)
			<-t.workers
		}()
	}
	for _, base := range themes {
		for _, theme := range []string{base, base + "-bars"} {
			t.startPostImageVariant(work, link, key, theme)
		}
	}
	return work
}

func (t *thumbnails) startPostImageVariant(work *postImagePreviewWork, link *url.URL, key, theme string) {
	if work.variants[theme] != nil {
		return
	}
	variant := &postImagePreviewVariant{done: make(chan struct{})}
	work.variants[theme] = variant
	work.active++
	work.finished = time.Time{}

	go func() {
		<-work.prepared
		if strings.HasSuffix(theme, "-bars") && work.duplicateBars() {
			t.mu.Lock()
			normal := work.variants[strings.TrimSuffix(theme, "-bars")]
			t.mu.Unlock()
			<-normal.done
			variant.request = normal.request
			variant.data, variant.preview, variant.thumbnail = normal.data, normal.preview, normal.thumbnail
			variant.width, variant.height = normal.width, normal.height
			variant.caption, variant.err = normal.caption, normal.err
			close(variant.done)
		} else {
			t.workers <- struct{}{}
			request := work.request
			if strings.HasSuffix(theme, "-bars") {
				request = work.bars
			}
			variant.render(t, link, request, theme, work.prepareErr)
			<-t.workers
		}
		t.mu.Lock()
		work.failed = work.failed || variant.err != nil
		work.active--
		if work.active == 0 {
			work.finished = time.Now()
			if work.failed {
				delete(t.postImageWork, key)
			}
		}
		t.mu.Unlock()
	}()
}

func (variant *postImagePreviewVariant) render(t *thumbnails, link *url.URL, request renderRequest, theme string, prepareErr error) {
	defer close(variant.done)
	variant.err = prepareErr
	if prepareErr != nil {
		return
	}
	request.Theme = strings.TrimSuffix(theme, "-bars")
	variant.request = request
	variant.caption = postImageCaption(link, request)
	variant.data, variant.err = renderPostImage(request)
	if variant.err != nil {
		return
	}
	blocks := []previewBlock{
		{Kind: "media", Image: variant.data, Video: request.hasMotion(), Count: 1},
		{Kind: "caption", Text: request.Post.AuthorName},
	}
	variant.thumbnail, variant.preview, variant.width, variant.height, variant.err = t.renderMessagePreview(blocks)
}

func (work *postImagePreviewWork) duplicateBars() bool {
	media := work.request.Post.Media
	return work.prepareErr == nil && media != nil && len(media.Items) == 0 && media.Carousel
}

func (t *thumbnails) duplicatePostImageBars(link *url.URL, zone string, translation translationSettings, work *postImagePreviewWork) bool {
	if work == nil {
		t.mu.Lock()
		work = t.postImageWork[artworkID(xPostLink(link))+":"+zone+":"+translation.key()]
		t.mu.Unlock()
	}
	if work != nil {
		select {
		case <-work.prepared:
			return work.duplicateBars()
		default:
		}
	}
	return false
}

func (work *postImagePreviewWork) postImageHasBars() bool {
	select {
	case <-work.prepared:
		return !work.barsKnown || work.hasBars
	case <-time.After(8 * time.Second):
		return true
	}
}

func (t *thumbnails) postImagePreview(link *url.URL, theme, zone string, translation translationSettings, work *postImagePreviewWork) previewPhoto {
	if work == nil {
		return t.postImagePlaceholder(strings.TrimSuffix(theme, "-bars"))
	}
	link = postImagePreviewLink(link)
	t.mu.Lock()
	variant := work.variants[theme]
	t.mu.Unlock()
	select {
	case <-variant.done:
		if variant.err == nil {
			address := *t.base
			address.Path += "/post-image-preview"
			address.RawQuery = url.Values{
				"url": {link.String()}, "theme": {theme},
				"tz": {zone}, "target": {translation.Target}, "ignored": {strings.SplitN(translation.key(), ":", 2)[1]}, "v": {strconv.Itoa(postImageVersion)}, "frame": {strconv.Itoa(thumbnailRenderVersion)},
			}.Encode()
			u := address.String()
			return previewPhoto{photoURL: u + "&size=large", thumbnailURL: u, width: variant.width, height: variant.height}
		}
		log.Printf("generating post image preview for %s (%s): %v", link, theme, variant.err)
	case <-time.After(8 * time.Second):
	}
	return t.postImagePlaceholder(strings.TrimSuffix(theme, "-bars"))
}

func (t *thumbnails) existingPostImagePreview(link *url.URL, theme, zone string, translation translationSettings) *postImagePreviewVariant {
	t.mu.Lock()
	defer t.mu.Unlock()
	if work := t.postImageWork[artworkID(xPostLink(link))+":"+zone+":"+translation.key()]; work != nil {
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
	zone := r.URL.Query().Get("tz")
	if zone == "" {
		zone = defaultTimezone
	}
	if _, err := time.LoadLocation(zone); err != nil {
		http.Error(w, "invalid timezone", http.StatusBadRequest)
		return
	}
	_, validTheme := themeOption(strings.TrimSuffix(theme, "-bars"))
	if link == nil || !xLinkHosts[strings.ToLower(link.Host)] || !validTheme {
		http.Error(w, "invalid URL or theme", http.StatusBadRequest)
		return
	}

	translation, ok := postImagePreviewTranslation(r.URL.Query())
	if !ok {
		http.Error(w, "invalid translation settings", http.StatusBadRequest)
		return
	}
	work := t.startPostImagePreview(link, zone, translation, strings.TrimSuffix(theme, "-bars"))
	t.mu.Lock()
	variant := work.variants[theme]
	t.mu.Unlock()
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
			data = variant.preview
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}
}

func postImagePreviewTranslation(query url.Values) (translationSettings, bool) {
	translation := defaultTranslationSettings()
	if query.Has("target") {
		translation.Target = query.Get("target")
	}
	if query.Has("ignored") {
		translation.Ignored = nil
		if query.Get("ignored") != "" {
			translation.Ignored = strings.Split(query.Get("ignored"), ",")
		}
	}
	if !validTranslationLanguage(translation.Target) {
		return translation, false
	}
	for _, code := range translation.Ignored {
		if !validTranslationLanguage(code) {
			return translation, false
		}
	}
	return translation, true
}
