package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type messagePreviewFixture struct {
	t       *testing.T
	server  *httptest.Server
	image   []byte
	answers chan []map[string]any
	edits   chan map[string]any
	calls   atomic.Int32
	mu      sync.Mutex
	uploads int
}

func newMessagePreviewFixture(t *testing.T) (*messagePreviewFixture, *thumbnails, *tgbotapi.BotAPI) {
	t.Helper()
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := range 60 {
		for x := range 80 {
			pixel := color.RGBA{R: uint8(x * 3), G: uint8(y * 4), B: 90, A: 255}
			img.Set(x, y, pixel)
		}
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, img, nil); err != nil {
		t.Fatal(err)
	}
	f := &messagePreviewFixture{
		t: t, image: data.Bytes(),
		answers: make(chan []map[string]any, 10), edits: make(chan map[string]any, 10),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	target, _ := url.Parse(f.server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	t.Cleanup(func() { http.DefaultTransport = original })
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", f.server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews := testPreviews(t, testArtworkDB(t), bot)
	previews.client = &http.Client{Transport: redirectTransport{target, original}}
	return f, previews, bot
}

func (f *messagePreviewFixture) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/user/status/"):
		f.calls.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/user/status/")

		tweet := map[string]any{
			"text":   "A distinctive post",
			"author": map[string]string{"name": "Artist", "screen_name": "user"},
		}
		photo := map[string]any{"type": "photo", "url": f.server.URL + "/image"}
		switch id {
		case "1":
			tweet["media"] = map[string]any{"all": []any{photo, photo}, "photos": []any{photo, photo}}
		case "2":
			tweet["media"] = map[string]any{"all": []any{photo}, "photos": []any{photo}}
		case "3":
			video := map[string]any{"type": "video", "url": f.server.URL + "/video", "thumbnail_url": f.server.URL + "/image"}
			tweet["media"] = map[string]any{"all": []any{video}}
		case "4":
			tweet["quote"] = map[string]any{
				"text":   "A distinctive quotation",
				"author": map[string]string{"name": "Quoted", "screen_name": "quoted"},
			}
		case "6":
			row := "✦" + strings.Repeat("\u2800", 29) + "⣿⣿⣿"
			tweet["text"] = strings.Join([]string{row, row, row, row}, " ")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
	case r.URL.Path == "/image":
		_, _ = w.Write(f.image)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"preview_bot"}}`)
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		_ = r.ParseForm()
		var results []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("results")), &results); err != nil {
			f.t.Error(err)
		}
		f.answers <- results
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"preview-file"}]}}`)
			return
		}
		_ = r.ParseForm()
		if r.FormValue("photo") != f.server.URL+"/image" {
			f.t.Errorf("unexpected original media: %q", r.FormValue("photo"))
		}
		f.mu.Lock()
		f.uploads++
		n := f.uploads
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"photo":[{"file_id":"original-%d"}]}}`, n, n)
	case strings.HasSuffix(r.URL.Path, "/sendMediaGroup"):
		_ = r.ParseForm()
		var media []struct{ Media string }
		if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
			f.t.Errorf("decoding original album: %v", err)
		}
		var messages []map[string]any
		f.mu.Lock()
		for _, item := range media {
			if item.Media != f.server.URL+"/image" {
				f.t.Errorf("unexpected original media: %q", item.Media)
			}
			f.uploads++
			photo := []map[string]string{{"file_id": fmt.Sprintf("original-%d", f.uploads)}}
			messages = append(messages, map[string]any{"message_id": f.uploads, "photo": photo})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": messages})
	case strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/deleteMessages"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/editMessageMedia"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
		_ = r.ParseForm()
		var payload map[string]any
		field := "media"
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			field = "rich_message"
		}
		if err := json.Unmarshal([]byte(r.FormValue(field)), &payload); err != nil {
			f.t.Errorf("edit %s: %v", field, err)
		}
		f.edits <- payload
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *messagePreviewFixture) query(t *testing.T, p *thumbnails, bot *tgbotapi.BotAPI, query string) []map[string]any {
	t.Helper()
	handleInlineQuery(bot, p.db, &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: query}, p)
	select {
	case answer := <-f.answers:
		return answer
	case <-time.After(2 * time.Second):
		t.Fatal("no inline answer")
		return nil
	}
}

func previewResult(t *testing.T, results []map[string]any, id string) map[string]any {
	t.Helper()
	for _, result := range results {
		if result["id"] == id {
			return result
		}
	}
	t.Fatalf("missing %s: %v", id, results)
	return nil
}

func servedMessagePreview(t *testing.T, p *thumbnails, result map[string]any, path string) []byte {
	t.Helper()
	raw, ok := result["photo_url"].(string)
	if !ok {
		t.Fatalf("missing photo URL: %v", result)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path != path {
		t.Fatalf("preview URL %q: %v", raw, err)
	}
	response := httptest.NewRecorder()
	p.ServeHTTP(response, httptest.NewRequest("GET", u.RequestURI(), nil))
	img, err := jpeg.Decode(bytes.NewReader(response.Body.Bytes()))
	if response.Code != 200 || err != nil {
		t.Fatalf("preview: status %d, jpeg %v", response.Code, err)
	}
	if result["photo_width"] != float64(img.Bounds().Dx()) || result["photo_height"] != float64(img.Bounds().Dy()) {
		t.Fatalf("dimensions %v, result %v", img.Bounds(), result)
	}
	return response.Body.Bytes()
}

func TestMessagePreviewInlineMedia(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	link := "https://x.com/user/status/1"

	results := f.query(t, p, bot, link)
	result := previewResult(t, results, inlineResultID)
	framed := servedMessagePreview(t, p, result, "/thumbnail")
	if bytes.Equal(framed, f.image) {
		t.Fatal("preview is original image, not a frame")
	}

	single := previewResult(t, f.query(t, p, bot, "https://x.com/user/status/2"), inlineResultID)
	singleFrame := servedMessagePreview(t, p, single, "/thumbnail")
	if bytes.Equal(framed, singleFrame) {
		t.Fatal("multi-photo preview does not show a slideshow")
	}
	selected := previewResult(t, f.query(t, p, bot, link+"/photo/1"), inlineResultID)
	selectedFrame := servedMessagePreview(t, p, selected, "/thumbnail")
	if !bytes.Equal(singleFrame, selectedFrame) {
		t.Fatal("selected photo preview shows extra slideshow pages")
	}

	content, ok := result["input_message_content"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(content["message_text"]), "Loading") {
		t.Fatalf("content: %v", result)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "inline", Query: link,
	}
	handleChosenInlineResult(bot, nil, p.db, chosen, "@cache", queue, p)
	select {
	case edit := <-f.edits:
		media := fmt.Sprint(edit["media"])
		if !strings.Contains(media, "original-") || strings.Contains(media, "preview") || !strings.Contains(fmt.Sprint(edit["html"]), "tg-slideshow") {
			t.Fatalf("sent content: %v", edit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no chosen edit")
	}

	for _, id := range []string{"x:1", "x:2", "x:1:photo:1"} {
		awaitCachedThumbnail(t, p.db, id, 0)
	}
}

func TestMessagePreviewVideoAndPostText(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	photo := previewResult(t, f.query(t, p, bot, "https://x.com/user/status/2"), inlineResultID)
	video := previewResult(t, f.query(t, p, bot, "https://x.com/user/status/3"), inlineResultID)
	a := servedMessagePreview(t, p, photo, "/thumbnail")
	b := servedMessagePreview(t, p, video, "/thumbnail")
	if bytes.Equal(a, b) {
		t.Fatal("video and photo previews are identical")
	}
	link := "https://x.com/user/status/4"
	regular := previewResult(t, f.query(t, p, bot, link), inlinePostTextID)
	r := servedMessagePreview(t, p, regular, "/post-text-preview")
	noQuote := previewResult(t, f.query(t, p, bot, "https://x.com/user/status/5"), inlinePostTextID)
	n := servedMessagePreview(t, p, noQuote, "/post-text-preview")
	if bytes.Equal(r, n) {
		t.Fatal("rich preview does not reflect quote")
	}
	for _, id := range []string{"x:2", "x:3"} {
		awaitCachedThumbnail(t, p.db, id, 0)
	}
}

func TestPostTextPreviewBrailleArt(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	result := previewResult(t, f.query(t, p, bot, "https://x.com/user/status/6"), inlinePostTextID)
	data := servedMessagePreview(t, p, result, "/post-text-preview")
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// All four stars must share a line with their dots, with blank cells between.
	hasInk := func(y, left, right int) bool {
		for x := left; x < right; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if min(r, g, b) > 0xc000 {
				return true
			}
		}
		return false
	}
	width := img.Bounds().Dx()
	rows, previous := 0, -10
	for y := range img.Bounds().Dy() {
		if hasInk(y, width/4, 3*width/4) {
			t.Fatal("Braille blank cells painted as visible glyphs")
		}
		if !hasInk(y, 0, width/4) || !hasInk(y, 3*width/4, width) {
			continue
		}
		if y-previous > 8 {
			rows++
		}
		previous = y
	}
	if rows != 4 {
		t.Fatalf("got %d intact Braille rows, want 4", rows)
	}
}

func TestMessagePreviewInstant(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	if _, err := p.db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}
	results := f.query(t, p, bot, "https://x.com/user/status/1")
	if f.calls.Load() != 1 {
		t.Fatalf("instant mode metadata requests = %d, want 1", f.calls.Load())
	}
	if !strings.Contains(fmt.Sprint(previewResult(t, results, inlineResultID)["photo_url"]), "placeholder") {
		t.Fatalf("instant result: %v", results)
	}
}

func TestMessagePreviewCachedPostImage(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	if _, err := p.db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}
	link := inlineLink("https://x.com/user/status/20")
	translation := defaultTranslationSettings()
	if err := savePostImage(p.db, link, "light", defaultTimezone, translation, "photo", "original-cached", "<b>Original caption</b>"); err != nil {
		t.Fatal(err)
	}
	results := f.query(t, p, bot, link.String())
	result := previewResult(t, results, postImageID(link, "light")+":cached")
	content := fmt.Sprint(result["input_message_content"])
	if !strings.Contains(content, "Loading post image") || strings.Contains(content, "original-cached") {
		t.Fatalf("cached post image: %v", result)
	}
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: result["id"].(string), InlineMessageID: "inline", Query: link.String(),
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	handleChosenInlineResult(bot, nil, p.db, chosen, "@cache", queue, p)
	assertCachedPreviewEdit(t, f)
	if f.calls.Load() != 1 {
		t.Fatalf("cached post image metadata requests = %d, want 1", f.calls.Load())
	}
}

func TestMessagePreviewCachedMerged(t *testing.T) {
	f, p, bot := newMessagePreviewFixture(t)
	a := artwork{
		ID: "x:21:merged", URL: "https://x.com/user/status/21", Author: "Cached",
		Caption: "<b>Original caption</b>", Photos: []string{"original-cached"},
	}
	if err := saveSelection(p.db, 1, a); err != nil {
		t.Fatal(err)
	}
	results := f.query(t, p, bot, "")
	result := previewResult(t, results, "x:21:merged:preview")
	if !strings.Contains(fmt.Sprint(result["input_message_content"]), "Loading merged") {
		t.Fatalf("cached merged: %v", result)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: result["id"].(string), InlineMessageID: "inline",
	}
	handleChosenInlineResult(bot, nil, p.db, chosen, "@cache", queue, p)
	assertCachedPreviewEdit(t, f)
}

func assertCachedPreviewEdit(t *testing.T, f *messagePreviewFixture) {
	t.Helper()
	select {
	case edit := <-f.edits:
		if edit["media"] != "original-cached" || edit["caption"] != "<b>Original caption</b>" {
			t.Fatalf("cached edit: %v", edit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no cached edit")
	}
}
