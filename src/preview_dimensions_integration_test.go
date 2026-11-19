package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
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

type previewFlowServer struct {
	t                            *testing.T
	server                       *httptest.Server
	images                       map[string][]byte
	answers                      chan []map[string]any
	sourceStarted, sourceRelease chan struct{}
	sourceOnce, startedOnce      sync.Once
	uploadStarted                chan struct{}
	uploadRelease                chan struct{}
	uploadFailed                 chan struct{}
	uploadOnce, releaseOnce      sync.Once
	failUpload                   atomic.Bool
	source3                      atomic.Int32
	sourceCalls, imageCalls      atomic.Int32
}

func newPreviewFlowServer(t *testing.T) *previewFlowServer {
	t.Helper()
	encode := func(img image.Image) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	f := &previewFlowServer{t: t, answers: make(chan []map[string]any, 10), sourceStarted: make(chan struct{}), sourceRelease: make(chan struct{}), uploadStarted: make(chan struct{}), uploadRelease: make(chan struct{}), uploadFailed: make(chan struct{}), images: map[string][]byte{
		"/portrait": thumbnailFixture(t, 1000, 1600),
		"/wide":     encode(image.NewRGBA(image.Rect(0, 0, 1001, 100))),
		"/boundary": encode(image.NewRGBA(image.Rect(0, 0, 1000, 100))),
		"/panel0":   encode(scene.SubImage(image.Rect(0, 0, 200, 320))),
		"/panel1":   encode(scene.SubImage(image.Rect(200, 0, 400, 320))),
	}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.sourceOnce.Do(func() { close(f.sourceRelease) })
		f.releaseOnce.Do(func() { close(f.uploadRelease) })
		f.server.Close()
	})
	return f
}

func (f *previewFlowServer) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/bottoken/getMe":
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case strings.HasPrefix(r.URL.Path, "/user/status/"):
		f.sourceCalls.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/user/status/"), "/en")
		if id == "7" {
			f.startedOnce.Do(func() { close(f.sourceStarted) })
			<-f.sourceRelease
		}

		if id == "3" {
			f.source3.Add(1)
		}

		paths := []string{"/portrait"}
		switch id {
		case "2":
			paths = []string{"/panel0", "/panel1"}
		case "4":
			paths = []string{"/portrait", "/panel0"}
		case "5":
			paths = []string{"/wide", "/wide"}
		case "6":
			paths = []string{"/boundary", "/boundary"}
		}
		photos := make([]map[string]string, 0, len(paths))
		for _, p := range paths {
			photos = append(photos, map[string]string{"type": "photo", "url": f.server.URL + p})
		}
		tweet := map[string]any{"author": map[string]string{"name": "Artist"}, "media": map[string]any{"photos": photos}}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
	case f.images[r.URL.Path] != nil:
		f.imageCalls.Add(1)
		_, _ = w.Write(f.images[r.URL.Path])
	case r.URL.Path == "/bottoken/answerInlineQuery":
		_ = r.ParseForm()
		var answer []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("results")), &answer); err != nil {
			f.t.Error(err)
		}
		f.answers <- answer
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case r.URL.Path == "/bottoken/sendPhoto":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			f.uploadOnce.Do(func() { close(f.uploadStarted) })
			<-f.uploadRelease
			if f.failUpload.Load() {
				close(f.uploadFailed)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"upload failed"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":2,"photo":[{"file_id":"preview-file"}]}}`)
		} else {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"art-file"}]}}`)
		}
	case strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/sendChatAction"), strings.HasSuffix(r.URL.Path, "/setMessageReaction"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *previewFlowServer) release() { f.releaseOnce.Do(func() { close(f.uploadRelease) }) }

func (f *previewFlowServer) setup(t *testing.T) (*thumbnails, *tgbotapi.BotAPI) {
	t.Helper()
	target, _ := url.Parse(f.server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	t.Cleanup(func() { http.DefaultTransport = original })
	db := testArtworkDB(t)
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", f.server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews, err := newThumbnails("https://preview.example.com", nil, bot, db, "@cache")
	if err != nil {
		t.Fatal(err)
	}
	return previews, bot
}

func (f *previewFlowServer) answer(t *testing.T, bot *tgbotapi.BotAPI, previews *thumbnails, id string) map[string]any {
	t.Helper()
	query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/" + id}
	handleInlineQuery(bot, previews.db, query, previews)
	return (<-f.answers)[0]
}

func TestPreviewInlineDimensions(t *testing.T) {
	for _, tc := range []struct{ id string }{{"1"}, {"2"}} {
		t.Run(tc.id, func(t *testing.T) {
			f := newPreviewFlowServer(t)
			previews, bot := f.setup(t)
			result := f.answer(t, bot, previews, tc.id)
			photo, thumb := uncachedThumbnailURLs(t, result)
			for i, raw := range []string{photo, thumb} {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest("GET", u.RequestURI(), nil)
				response := httptest.NewRecorder()
				previews.ServeHTTP(response, request)
				img, format, err := image.Decode(bytes.NewReader(response.Body.Bytes()))
				if response.Code != 200 || err != nil || format != "jpeg" {
					t.Fatalf("image %d: %d %s %v", i, response.Code, format, err)
				}
				size := img.Bounds().Size()
				if i == 0 && (result["photo_width"] != float64(size.X) || result["photo_height"] != float64(size.Y)) || i == 1 && max(size.X, size.Y) != 384 {
					t.Fatalf("image %d dimensions %v, result %#v", i, size, result)
				}
			}
			f.release()
			awaitCachedThumbnail(t, previews.db, "x:"+tc.id, 0)
			awaitCachedThumbnail(t, previews.db, "x:"+tc.id, 1)
		})
	}
}

func TestHistoryPreviewDoesNotWait(t *testing.T) {
	f := newPreviewFlowServer(t)
	previews, bot := f.setup(t)
	a := artwork{ID: "x:7", URL: "https://x.com/user/status/7", Photos: []string{"art-file"}}
	if err := saveSelection(previews.db, 1, a); err != nil {
		t.Fatal(err)
	}
	if err := savePreviewDimensions(previews.db, a.ID, 800, 1280); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]any, 1)
	go func() {
		query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}}
		handleInlineQuery(bot, previews.db, query, previews)
		done <- (<-f.answers)[0]
	}()
	select {
	case result := <-done:
		if result["photo_url"] != "https://preview.example.com/thumbnail-placeholder-x.jpg" || !strings.Contains(fmt.Sprint(result["input_message_content"]), "art-file") {
			t.Fatalf("history dimensions: %#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("history waited for the source")
	}
	f.sourceOnce.Do(func() { close(f.sourceRelease) })
	f.release()
	awaitCachedThumbnail(t, previews.db, a.ID, 1)
}

func TestPreviewWarmupAfterSave(t *testing.T) {
	for _, mode := range []string{"chat", "inline", "inline-after-preview"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreviewFlowServer(t)
			previews, bot := f.setup(t)

			queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
			link := "https://x.com/user/status/3"
			if mode == "inline-after-preview" {
				_ = f.answer(t, bot, previews, "3")
			}
			done := make(chan struct{})
			go func() {
				if mode == "chat" {
					message := &tgbotapi.Message{
						MessageID: 1, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1},
						Text: link, Entities: []tgbotapi.MessageEntity{{Type: "url", Length: len(link)}},
					}
					handleMessage(bot, nil, previews.db, message, queue, previews)
				} else {
					chosen := &tgbotapi.ChosenInlineResult{
						From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "inline", Query: link,
					}
					handleChosenInlineResult(bot, nil, previews.db, chosen, "@cache", queue, previews)
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("save waited for preview upload")
			}
			select {
			case <-f.uploadStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("preview upload did not start")
			}
			work := previews.start(inlineLink(link))
			<-work.done
			if work.err != nil {
				t.Fatal(work.err)
			}
			frame, _, err := image.Decode(bytes.NewReader(work.large))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				a, err := findArtwork(previews.db, "x:3")
				if err != nil {
					t.Fatal(err)
				}
				if a != nil && a.PreviewWidth == frame.Bounds().Dx() && a.PreviewHeight == frame.Bounds().Dy() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("dimensions not saved: %+v", a)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "inline-after-preview" && f.source3.Load() != 4 {
				t.Fatalf("source requests with existing preview: %d", f.source3.Load())
			}
			f.release()
			awaitCachedThumbnail(t, previews.db, "x:3", 1)
		})
	}
}

func TestPreviewUploadRetry(t *testing.T) {
	f := newPreviewFlowServer(t)
	previews, _ := f.setup(t)
	f.release()
	link := inlineLink("https://x.com/user/status/8")
	f.failUpload.Store(true)
	previews.warmup(link, 1)
	select {
	case <-f.uploadFailed:
	case <-time.After(5 * time.Second):
		t.Fatal("failed upload did not start")
	}
	f.failUpload.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		previews.warmup(link, 1)
		id, err := findThumbnail(previews.db, "x:8", 1)
		if err != nil {
			t.Fatal(err)
		}
		if id == "preview-file" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("preview upload was not retried")
}

func TestPreviewInlineTimeoutCompletesInBackground(t *testing.T) {
	f := newPreviewFlowServer(t)
	previews, bot := f.setup(t)
	done := make(chan map[string]any, 1)
	go func() { done <- f.answer(t, bot, previews, "7") }()
	select {
	case <-f.sourceStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("source not started")
	}
	var result map[string]any
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("inline answer did not time out")
	}
	if result["id"] != inlineResultID || !strings.Contains(fmt.Sprint(result["input_message_content"]), "https://x.com/user/status/7") {
		t.Fatalf("expected selectable artwork fallback: %#v", result)
	}
	f.sourceOnce.Do(func() { close(f.sourceRelease) })
	f.release()
	awaitCachedThumbnail(t, previews.db, "x:7", 0)
}

func TestMergedInlinePreview(t *testing.T) {
	for _, tc := range []struct {
		id     string
		actual int
		size   image.Point
	}{{"2", 1, image.Pt(400, 320)}, {"4", 2, image.Pt(1280, 1024)}} {
		t.Run(tc.id, func(t *testing.T) {
			f := newPreviewFlowServer(t)
			previews, bot := f.setup(t)

			query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/" + tc.id}
			handleInlineQuery(bot, previews.db, query, previews)
			results := <-f.answers
			if len(results) != 8 || results[7]["id"] != "post:text" || results[1]["id"] != inlineMergedResultID {
				t.Fatalf("results: %#v", results)
			}

			barsURL, err := url.Parse(results[2]["photo_url"].(string))
			if err != nil || results[2]["id"] != inlineBarsResultID || barsURL.Query().Get("merge") != "bars" {
				t.Fatalf("bars result: %#v", results[2])
			}
			barsResponse := httptest.NewRecorder()
			previews.ServeHTTP(barsResponse, httptest.NewRequest("GET", barsURL.RequestURI(), nil))
			barsImage, _, err := image.Decode(bytes.NewReader(barsResponse.Body.Bytes()))
			if barsResponse.Code != 200 || err != nil || results[2]["photo_width"] != float64(barsImage.Bounds().Dx()) || results[2]["photo_height"] != float64(barsImage.Bounds().Dy()) {
				t.Fatalf("bars preview: %d %v", barsResponse.Code, err)
			}

			u, err := url.Parse(results[1]["photo_url"].(string))
			if err != nil || u.Query().Get("merge") != "1" {
				t.Fatalf("merged URL: %s", u)
			}
			response := httptest.NewRecorder()
			previews.ServeHTTP(response, httptest.NewRequest("GET", u.RequestURI(), nil))
			img, format, err := image.Decode(bytes.NewReader(response.Body.Bytes()))
			if response.Code != 200 || err != nil || format != "jpeg" {
				t.Fatalf("merged image: %d %s %v", response.Code, format, err)
			}
			if results[1]["photo_width"] != float64(img.Bounds().Dx()) || results[1]["photo_height"] != float64(img.Bounds().Dy()) {
				t.Fatalf("merged dimensions: %v, result: %#v", img.Bounds().Size(), results[1])
			}

			work := previews.start(inlineLink(query.Query))
			<-work.done
			if f.sourceCalls.Load() != 3 || f.imageCalls.Load() != 3 {
				t.Fatalf("requests: source=%d images=%d", f.sourceCalls.Load(), f.imageCalls.Load())
			}
			if work.actual != tc.actual || work.photoCount != 2 {
				t.Fatalf("source: actual=%d photos=%d", work.actual, work.photoCount)
			}
			f.release()
			awaitCachedThumbnail(t, previews.db, "x:"+tc.id, 0)
		})
	}
}

func TestMergedInlinePhotoDimensionsFilter(t *testing.T) {
	for _, tc := range []struct {
		id           string
		merged, bars bool
	}{
		{"5", false, false},
		{"6", true, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			f := newPreviewFlowServer(t)
			previews, bot := f.setup(t)
			query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/" + tc.id}
			handleInlineQuery(bot, previews.db, query, previews)
			results := <-f.answers
			found := map[string]bool{}
			for _, result := range results {
				found[result["id"].(string)] = true
			}
			if found[inlineMergedResultID] != tc.merged || found[inlineBarsResultID] != tc.bars || !found[inlineResultID] || !found["post:text"] {
				t.Fatalf("inline results: %#v", results)
			}
			for _, bars := range []bool{false, true} {
				photo, ok := previews.mergedPhotoVariant(inlineLink(query.Query), true, bars)
				if !ok || !strings.Contains(photo.photoURL, "/thumbnail-placeholder-merged") {
					t.Fatalf("instant merged preview (bars=%v): %#v %v", bars, photo, ok)
				}
			}
			f.release()
			awaitCachedThumbnail(t, previews.db, "x:"+tc.id, 0)
		})
	}
}

func TestMergedInlineSingletonAndInstant(t *testing.T) {
	f := newPreviewFlowServer(t)
	previews, bot := f.setup(t)
	link := inlineLink("https://x.com/user/status/1")
	photo, ok := previews.mergedPhoto(inlineLink("https://x.com/user/status/2"), true)
	if !ok || !strings.HasSuffix(photo.photoURL, "/thumbnail-placeholder-merged.jpg") {
		t.Fatalf("instant preview: %#v %v", photo, ok)
	}
	response := httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", photo.photoURL, nil))
	if _, format, err := image.Decode(bytes.NewReader(response.Body.Bytes())); response.Code != 200 || err != nil || format != "jpeg" {
		t.Fatalf("placeholder: %d %s %v", response.Code, format, err)
	}
	if _, ok := previews.mergedPhoto(inlineLink("https://x.com/user/status/1/photo/1"), true); ok {
		t.Fatal("selected photo offered merge")
	}
	a := artwork{ID: "x:1", URL: link.String(), Photos: []string{"art-file"}}
	if err := saveSelection(previews.db, 1, a); err != nil {
		t.Fatal(err)
	}
	query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: link.String()}
	handleInlineQuery(bot, previews.db, query, previews)
	if results := <-f.answers; len(results) != 6 || results[5]["id"] != "post:text" {
		t.Fatalf("cached singleton: %#v", results)
	}
	if _, ok := previews.mergedPhoto(link, true); ok {
		t.Fatal("known singleton offered instant merge")
	}
	f.release()
	awaitCachedThumbnail(t, previews.db, "x:1", 0)
	awaitCachedThumbnail(t, previews.db, "x:1", 1)
}
