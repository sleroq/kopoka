package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

type offlinePreviewTransport struct{}

func (offlinePreviewTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("offline preview")
}

func testPreviews(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI) *thumbnails {
	t.Helper()
	previews, err := newThumbnails("https://preview.example.com", nil, bot, db, "@cache")
	if err != nil {
		t.Fatal(err)
	}
	previews.client = &http.Client{Transport: offlinePreviewTransport{}}
	return previews
}

func thumbnailFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{80, 170, 160, 255}}, image.Point{}, draw.Src)
	edges := []struct {
		bounds image.Rectangle
		color  color.RGBA
	}{
		{image.Rect(0, 0, width/12, height), color.RGBA{220, 40, 40, 255}},
		{image.Rect(width-width/12, 0, width, height), color.RGBA{40, 70, 220, 255}},
		{image.Rect(0, 0, width, height/12), color.RGBA{240, 180, 30, 255}},
		{image.Rect(0, height-height/12, width, height), color.RGBA{120, 40, 180, 255}},
	}
	for _, edge := range edges {
		draw.Draw(img, edge.bounds, &image.Uniform{C: edge.color}, image.Point{}, draw.Src)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

type thumbnailTestServer struct {
	t                                                     *testing.T
	server                                                *httptest.Server
	fixture                                               []byte
	started, release, uploadStarted, uploadRelease        chan struct{}
	startOnce, releaseOnce, uploadOnce, uploadReleaseOnce sync.Once
	upstream, failures, uploads, downloads                atomic.Int32
	answers                                               chan []map[string]any
}

func newThumbnailTestServer(t *testing.T) *thumbnailTestServer {
	t.Helper()
	s := &thumbnailTestServer{
		t: t, fixture: thumbnailFixture(t, 1600, 1000), answers: make(chan []map[string]any, 10),
		started: make(chan struct{}), release: make(chan struct{}),
		uploadStarted: make(chan struct{}), uploadRelease: make(chan struct{}),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

func (s *thumbnailTestServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		_ = r.ParseForm()

		var result []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("results")), &result); err != nil {
			s.t.Error(err)
		}
		s.answers <- result
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			s.t.Error(err)
		}
		if r.FormValue("chat_id") != "@cache" {
			s.t.Errorf("cache chat = %q", r.FormValue("chat_id"))
		}
		file, _, err := r.FormFile("photo")
		if err != nil {
			s.t.Error(err)
		} else {
			img, format, err := image.Decode(file)
			_ = file.Close()
			if err != nil || format != "jpeg" {
				s.t.Errorf("uploaded image: format %s, error %v", format, err)
			} else if img.Bounds().Size() != image.Pt(1280, 800) {
				s.t.Errorf("uploaded dimensions: %v", img.Bounds().Size())
			}
		}
		n := s.uploads.Add(1)
		if n == 1 {
			s.uploadOnce.Do(func() { close(s.uploadStarted) })
			<-s.uploadRelease
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":1}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"photo":[{"file_id":"thumbnail-%d"}]}}`, n, n)
	case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
		if err := r.ParseForm(); err != nil {
			s.t.Error(err)
		}
		if r.FormValue("chat_id") != "@cache" {
			s.t.Errorf("delete cache chat = %q", r.FormValue("chat_id"))
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case r.URL.Path == "/user/status/1":
		s.upstream.Add(1)
		s.startOnce.Do(func() { close(s.started) })
		<-s.release
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{"media": map[string]any{"all": []any{
			map[string]any{"type": "video", "url": s.server.URL + "/video.mp4", "thumbnail_url": s.server.URL + "/cover.png"},
			map[string]any{"type": "photo", "url": s.server.URL + "/cover.png"},
		}}}})
	case r.URL.Path == "/user/status/2":
		s.upstream.Add(1)
		if s.failures.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{"media": map[string]any{
			"photos": []any{map[string]any{"type": "photo", "url": s.server.URL + "/small.png"}},
		}}})
	case r.URL.Path == "/cover.png":
		s.downloads.Add(1)
		_, _ = w.Write(s.fixture)
	case r.URL.Path == "/small.png":
		_, _ = w.Write(thumbnailFixture(s.t, 480, 300))
	default:
		http.NotFound(w, r)
	}
}

func TestInlineThumbnailFlow(t *testing.T) {
	s := newThumbnailTestServer(t)
	defer s.server.Close()
	target, _ := url.Parse(s.server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", s.server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	path := legacyThumbnailDB(t)
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE thumbnail_cache SET render_version=4 WHERE artwork_id='x:1'`); err != nil {
		t.Fatal(err)
	}
	previews, err := newThumbnails("https://preview.example.com", nil, bot, db, "@cache")
	if err != nil {
		t.Fatal(err)
	}
	previewServer := httptest.NewServer(previews)
	defer previewServer.Close()
	defer func() {
		s.releaseOnce.Do(func() { close(s.release) })
		s.uploadReleaseOnce.Do(func() { close(s.uploadRelease) })
	}()
	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/1"}

	answered := make(chan struct{})
	go func() {
		handleInlineQuery(bot, db, query, previews)
		close(answered)
	}()
	select {
	case <-s.started:
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail generation did not start")
	}
	select {
	case <-answered:
		t.Fatal("answered before generation finished")
	default:
	}
	s.releaseOnce.Do(func() { close(s.release) })
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("answer waited for upload")
	}
	result := (<-s.answers)[0]
	if result["photo_width"] != float64(1280) || result["photo_height"] != float64(800) {
		t.Fatalf("dimensions: %#v", result)
	}
	photoURL, thumbnailURL := uncachedThumbnailURLs(t, result)
	localURL := previewServer.URL + strings.TrimPrefix(thumbnailURL, "https://preview.example.com")
	largeURL := previewServer.URL + strings.TrimPrefix(photoURL, "https://preview.example.com")
	var images []image.Image
	for _, endpoint := range []string{localURL, largeURL} {
		res, err := http.Get(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		images = append(images, readThumbnail(t, res))
	}
	multi, large := images[0], images[1]
	if large.Bounds().Size() != image.Pt(1280, 800) {
		t.Fatalf("landscape photo size: %v", large.Bounds().Size())
	}
	checkThumbnailEdges(t, large)
	checkXDecorations(t, large)
	if multi.Bounds().Size() != image.Pt(384, 240) {
		t.Fatalf("landscape thumbnail size: %v", multi.Bounds().Size())
	}
	checkThumbnailEdges(t, multi)
	checkXDecorations(t, multi)
	select {
	case <-s.uploadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail upload did not start")
	}
	s.uploadReleaseOnce.Do(func() { close(s.uploadRelease) })
	db = reopenCachedThumbnail(t, s, bot, db, path, previews, query)

	checkCachedThumbnails(t, db, bot, previews, s.answers, previewServer, multi)
	awaitCachedThumbnail(t, db, "x:1", 1)
	checkInstantThumbnail(t, db, bot, previews, s.answers, previewServer, &s.upstream)
	handleInlineQuery(bot, db, query, previews)
	if result := (<-s.answers)[0]; result["photo_file_id"] != "thumbnail-2" {
		t.Fatalf("instant cached thumbnail: %#v", result)
	}
	checkInvalidArtworkThumbnails(t, db, bot, previews, s.answers)
	checkCachedSourceFailure(t, db, bot, previews, s.answers, previewServer)
	checkThumbnailFailures(t, db, previewServer.URL, "https://x.com/user/status/2", &s.uploads)
}

func reopenCachedThumbnail(t *testing.T, s *thumbnailTestServer, bot *tgbotapi.BotAPI, db *sql.DB, path string, previews *thumbnails, query *tgbotapi.InlineQuery) *sql.DB {
	t.Helper()

	awaitCachedThumbnail(t, db, "x:1", 0)
	awaitCachedThumbnail(t, db, "x:1", 2)
	var version int
	row := db.QueryRow(`SELECT render_version FROM thumbnail_cache WHERE artwork_id='x:1' AND media_count=0`)
	err := row.Scan(&version)
	if err != nil || version != thumbnailRenderVersion {
		t.Fatalf("cached render version: %d, %v", version, err)
	}
	if err := toggleInstantInline(db, query.From.ID, "x"); err != nil {
		t.Fatal(err)
	}
	before := s.upstream.Load()
	handleInlineQuery(bot, db, query, previews)
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" || cached["photo_url"] != nil || cached["thumbnail_url"] != nil {
		t.Fatalf("cached thumbnail: %#v", cached)
	}
	if s.upstream.Load() != before {
		t.Fatal("cached query fetched source")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	previews.db = db
	handleInlineQuery(bot, db, query, previews)
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" || cached["photo_url"] != nil || cached["thumbnail_url"] != nil {
		t.Fatalf("reopened thumbnail: %#v", cached)
	}
	query.Query = "https://vxtwitter.com/other/status/1"
	handleInlineQuery(bot, db, query, previews)
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" {
		t.Fatalf("thumbnail via alternate host: %#v", cached)
	}
	query.Query = "https://x.com/user/status/1"
	if s.upstream.Load() != before || s.downloads.Load() != 2 {
		t.Fatalf("metadata requests = %d (before %d), image downloads = %d; want no new metadata and 2 downloads",
			s.upstream.Load(), before, s.downloads.Load())
	}
	if err := toggleInstantInline(db, query.From.ID, "x"); err != nil {
		t.Fatal(err)
	}
	return db
}

func legacyThumbnailDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thumbnails.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE thumbnail_cache (artwork_id TEXT NOT NULL, media_count INTEGER NOT NULL,
		file_id TEXT NOT NULL, PRIMARY KEY (artwork_id, media_count));
		INSERT INTO thumbnail_cache VALUES ('x:1', 0, 'old-square-thumbnail')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func uncachedThumbnailURLs(t *testing.T, result map[string]any) (string, string) {
	t.Helper()
	photo, pok := result["photo_url"].(string)
	thumbnail, tok := result["thumbnail_url"].(string)
	p, pe := url.Parse(photo)
	thumb, te := url.Parse(thumbnail)
	if !pok || !tok || pe != nil || te != nil || result["type"] != "photo" || p.Scheme != "https" || p.Host != "preview.example.com" || p.Path != "/thumbnail" || thumb.Scheme != p.Scheme || thumb.Host != p.Host || thumb.Path != p.Path {
		t.Fatalf("inline result: %#v", result)
	}
	pq, tq := p.Query(), thumb.Query()
	if pq.Get("size") != "large" || tq.Has("size") || tq.Get("url") == "" || tq.Get("v") != fmt.Sprint(thumbnailRenderVersion) {
		t.Fatalf("preview queries: %v, %v", pq, tq)
	}
	pq.Del("size")
	if !reflect.DeepEqual(pq, tq) {
		t.Fatalf("preview queries differ: %v, %v", pq, tq)
	}
	if result["photo_width"] == nil || result["photo_height"] == nil {
		t.Fatalf("missing dimensions: %#v", result)
	}
	return photo, thumbnail
}

func awaitCachedThumbnail(t *testing.T, db *sql.DB, id string, count int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fileID, err := findThumbnail(db, id, count)
		if err != nil {
			t.Fatal(err)
		}
		if fileID != "" {
			return fileID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("thumbnail %s count %d not cached", id, count)
	return ""
}

func checkInstantThumbnail(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI, previews *thumbnails, answers <-chan []map[string]any, server *httptest.Server, upstream *atomic.Int32) {
	t.Helper()
	if err := toggleInstantInline(db, 1, "x"); err != nil {
		t.Fatal(err)
	}
	before := upstream.Load()
	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/2"}
	handleInlineQuery(bot, db, query, previews)
	if result := (<-answers)[0]; result["photo_url"] != "https://preview.example.com/thumbnail-placeholder-x.jpg" || result["thumbnail_url"] != result["photo_url"] || upstream.Load() != before {
		t.Fatal("instant inline mode started a preview")
	}
	for _, source := range []string{"x", "pixiv"} {
		res, err := http.Get(server.URL + "/thumbnail-placeholder-" + source + ".jpg")
		if err != nil {
			t.Fatal(err)
		}
		_ = readThumbnail(t, res)
	}
	if upstream.Load() != before {
		t.Fatal("placeholder contacted an upstream source")
	}
}

func checkInvalidArtworkThumbnails(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI, previews *thumbnails, answers <-chan []map[string]any) {
	t.Helper()
	for _, tc := range []struct{ kind, fileID, photoURL string }{
		{"photo", "art-photo", ""}, {"video", "", "https://preview.example.com/thumbnail-placeholder-x.jpg"},
	} {
		a := artwork{ID: "x:invalid", URL: "invalid", Photos: []string{"art-" + tc.kind}, Types: []string{tc.kind}}
		if err := saveSelection(db, 1, a); err != nil {
			t.Fatal(err)
		}
		handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "history", From: &tgbotapi.User{ID: 1}}, previews)
		result := (<-answers)[0]
		if tc.fileID != "" && result["photo_file_id"] != tc.fileID || tc.photoURL != "" && result["photo_url"] != tc.photoURL {
			t.Fatalf("invalid source %s: %#v", tc.kind, result)
		}
	}
}

func checkCachedSourceFailure(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI, previews *thumbnails, answers <-chan []map[string]any, server *httptest.Server) {
	t.Helper()
	a := artwork{ID: "x:3", URL: "https://x.com/user/status/3", Author: "Artist", Photos: []string{"saved-photo"}}
	if err := saveSelection(db, 1, a); err != nil {
		t.Fatal(err)
	}
	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: 1}, Query: a.URL}
	handleInlineQuery(bot, db, query, previews)
	result := (<-answers)[0]
	rich := result["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	media := rich["media"].([]any)[0].(map[string]any)["media"].(map[string]any)
	if result["type"] != "photo" || media["media"] != "saved-photo" {
		t.Fatalf("cached artwork: %#v", result)
	}
	if result["photo_file_id"] != "saved-photo" {
		t.Fatalf("fallback: %#v", result)
	}
	res, err := http.Get(server.URL + "/thumbnail?url=" + url.QueryEscape(a.URL) + "&size=large")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %d", res.StatusCode)
	}
}

func checkThumbnailFailures(t *testing.T, db *sql.DB, baseURL, sourceURL string, uploads *atomic.Int32) {
	t.Helper()
	res, err := http.Get(baseURL + "/thumbnail?url=https%3A%2F%2Fexample.com%2Fimage")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid source status = %d", res.StatusCode)
	}

	before := uploads.Load()
	retryURL := baseURL + "/thumbnail?url=" + url.QueryEscape(sourceURL) + "&count=5"
	res, err = http.Get(retryURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed source status = %d", res.StatusCode)
	}

	res, err = http.Get(retryURL)
	if err != nil {
		t.Fatal(err)
	}
	small := readThumbnail(t, res)
	if small.Bounds().Size() != image.Pt(384, 240) {
		t.Fatalf("small source thumbnail: %v", small.Bounds().Size())
	}
	res, err = http.Get(retryURL + "&size=large")
	if err != nil {
		t.Fatal(err)
	}
	large := readThumbnail(t, res)
	if large.Bounds().Size() != image.Pt(480, 300) {
		t.Fatalf("small source photo: %v", large.Bounds().Size())
	}

	res, err = http.Get(retryURL + "&size=invalid")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid size status: %d", res.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)
	id, err := findThumbnail(db, "x:2", 5)
	if err != nil || id != "" || uploads.Load() != before {
		t.Fatalf("direct request cached a thumbnail: id %q, uploads %d, error %v", id, uploads.Load()-before, err)
	}
}

func checkCachedThumbnails(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI, previews *thumbnails, answers <-chan []map[string]any, previewServer *httptest.Server, multi image.Image) {
	t.Helper()
	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/1"}
	a := artwork{
		ID: "x:1", URL: query.Query, Author: "Artist", Caption: `<a href="https://x.com/user/status/1">Artist</a>`,
		Photos: []string{"video-id"}, Types: []string{"video"},
	}
	if err := saveSelection(db, 1, a); err != nil {
		t.Fatal(err)
	}
	if err := savePreviewDimensions(db, a.ID, 1280, 800); err != nil {
		t.Fatal(err)
	}
	handleInlineQuery(bot, db, query, previews)
	cached := (<-answers)[0]
	rich := cached["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	media := rich["media"].([]any)[0].(map[string]any)["media"].(map[string]any)
	if cached["type"] != "photo" || cached["id"] != "x:1" || media["type"] != "video" || media["media"] != "video-id" || !strings.Contains(rich["html"].(string), "tg://video?id=0") {
		t.Fatalf("cached single result: %#v", cached)
	}
	if cached["photo_width"] != float64(1280) || cached["photo_height"] != float64(800) {
		t.Fatalf("cached artwork dimensions: %#v", cached)
	}
	cachedURL := cached["thumbnail_url"].(string)
	if !strings.Contains(cachedURL, "count=1") {
		t.Fatalf("cached thumbnail URL: %s", cachedURL)
	}
	res, err := http.Get(previewServer.URL + strings.TrimPrefix(cachedURL, "https://preview.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	single := readThumbnail(t, res)
	if !reflect.DeepEqual(single, multi) {
		t.Fatal("cache-key count changed the source-count preview")
	}
	size := single.Bounds().Size()
	side := min(size.X, size.Y)
	r, g, b, _ := single.At(size.X-87*side/384, size.Y-87*side/384).RGBA()
	if r+g+b < 10000 {
		t.Fatal("X logo tile corners are not rounded")
	}

	a.Photos, a.Types = []string{"photo-id"}, []string{"photo"}
	if err := saveSelection(db, 1, a); err != nil {
		t.Fatal(err)
	}
	handleInlineQuery(bot, db, query, previews)
	cached = (<-answers)[0]
	rich = cached["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	media = rich["media"].([]any)[0].(map[string]any)["media"].(map[string]any)
	if media["type"] != "photo" || media["media"] != "photo-id" || !strings.Contains(rich["html"].(string), "tg://photo?id=0") {
		t.Fatalf("cached photo result: %#v", cached)
	}

	a.Photos = []string{"video-id", "photo-id"}
	a.Types = []string{"video", "photo"}
	if err := saveSelection(db, 1, a); err != nil {
		t.Fatal(err)
	}
	query.Query = ""
	handleInlineQuery(bot, db, query, previews)
	cached = (<-answers)[0]
	rich = cached["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	if !strings.Contains(rich["html"].(string), "<tg-slideshow>") || len(rich["media"].([]any)) != 2 || cached["photo_file_id"] != "thumbnail-2" || rich["media"].([]any)[0].(map[string]any)["media"].(map[string]any)["media"] != "video-id" || rich["media"].([]any)[1].(map[string]any)["media"].(map[string]any)["media"] != "photo-id" {
		t.Fatalf("cached slideshow result: %#v", cached)
	}
}

func readThumbnail(t *testing.T, res *http.Response) image.Image {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail: %s, type %s", res.Status, res.Header.Get("Content-Type"))
	}
	img, format, err := image.Decode(res.Body)
	if err != nil || format != "jpeg" {
		t.Fatalf("thumbnail format: %s, %v", format, err)
	}
	return img
}

func checkThumbnailEdges(t *testing.T, img image.Image) {
	t.Helper()
	size := img.Bounds().Size()
	leftR, leftG, leftB, _ := img.At(5, size.Y/2).RGBA()
	rightR, rightG, rightB, _ := img.At(size.X-6, size.Y/2).RGBA()
	topR, topG, topB, _ := img.At(size.X/2, 5).RGBA()
	bottomR, bottomG, bottomB, _ := img.At(size.X/2, size.Y-6).RGBA()
	if leftR <= leftG || leftR <= leftB || rightB <= rightR || rightB <= rightG ||
		topR <= topB || topG <= topB || bottomB <= bottomR || bottomB <= bottomG {
		t.Fatal("thumbnail cropped an artwork edge")
	}
}

func checkXDecorations(t *testing.T, img image.Image) {
	t.Helper()
	size := img.Bounds().Size()
	side := min(size.X, size.Y)
	r, g, b, _ := img.At(size.X-74*side/384, size.Y-54*side/384).RGBA()
	if r+g+b > 10000 {
		t.Fatal("X logo tile is missing")
	}
	r, g, b, _ = img.At(size.X-46*side/384, 20*side/384).RGBA()
	if r+g+b > 30000 {
		t.Fatal("count badge is missing")
	}
}

func TestPixivThumbnailFlow(t *testing.T) {
	fixture, err := os.ReadFile("testdata/thumbnail-portrait.webp")
	if err != nil {
		t.Fatal(err)
	}
	var previews *thumbnails
	var downloads, metadata atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/thumbnail":
			previews.ServeHTTP(w, r)
		case "/bottoken/getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case "/bottoken/sendPhoto":
			_ = r.ParseMultipartForm(8 << 20)
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
			} else {
				img, format, err := image.Decode(file)
				_ = file.Close()
				if err != nil || format != "jpeg" {
					t.Errorf("Pixiv upload format %s, error %v", format, err)
				} else if img.Bounds().Size() != image.Pt(800, 1280) {
					t.Errorf("Pixiv upload dimensions: %v", img.Bounds().Size())
				}
			}
			if r.FormValue("chat_id") != "@cache" {
				t.Errorf("cache chat: %q", r.FormValue("chat_id"))
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"pixiv-thumbnail"}]}}`)
		case "/bottoken/deleteMessage":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/auth/token":
			_, _ = fmt.Fprint(w, `{"response":{"access_token":"access","refresh_token":"refresh","expires_in":3600,"user":{}}}`)
		case "/v1/illust/detail":
			metadata.Add(1)
			servePixivThumbnailDetail(w, r)
		case "/p0", "/p1":
			downloads.Add(1)
			scene := artScene(image.Rect(0, 0, 400, 320), 0)
			x := int(r.URL.Path[2]-'0') * 200
			_ = png.Encode(w, scene.SubImage(image.Rect(x, 0, x+200, 320)))
		case "/cover.jpg":
			if r.Referer() != "https://www.pixiv.net/" {
				t.Error("missing Pixiv image referer")
			}
			downloads.Add(1)
			_, _ = w.Write(fixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base := server.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.ServerName = "127.0.0.1"
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient, http.DefaultTransport = &http.Client{}, base
	t.Cleanup(func() {
		http.DefaultClient.Transport.(*pixivTransport).pixiv.CloseIdleConnections()
		base.CloseIdleConnections()
		http.DefaultClient, http.DefaultTransport = oldClient, oldTransport
	})
	t.Setenv("PIXIV_ACCESS_TOKEN", "access")
	t.Setenv("PIXIV_REFRESH_TOKEN", "refresh")
	t.Setenv("PIXIV_TOKEN_DEADLINE", "3600")
	app, err := setupPixivClient()
	if err != nil {
		t.Fatal(err)
	}
	db := testArtworkDB(t)
	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	previews, err = newThumbnails(server.URL, app, bot, db, "@cache")
	if err != nil {
		t.Fatal(err)
	}
	link := inlineLink("https://www.pixiv.net/en/artworks/123")
	thumbnailURL := previews.url(link, 0)
	photoURL := thumbnailURL + "&size=large"
	checkPixivPreviews(t, server.Client(), photoURL, thumbnailURL)
	awaitCachedThumbnail(t, db, "pixiv:123", 0)
	awaitCachedThumbnail(t, db, "pixiv:123", 12)
	if downloads.Load() != 1 || metadata.Load() != 1 {
		t.Fatalf("Pixiv metadata requests: %d, image downloads: %d", metadata.Load(), downloads.Load())
	}
	checkPixivMergedPreview(t, previews, &downloads)
}

func servePixivThumbnailDetail(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("illust_id") == "124" {
		_, _ = fmt.Fprint(w, `{"illust":{"id":124,"type":"illust","page_count":1,
            "image_urls":{"large":"https://i.pximg.net/cover.jpg"},
            "meta_pages":[{"image_urls":{"original":"https://i.pximg.net/cover.jpg","large":"https://i.pximg.net/p0"}},
            {"image_urls":{"original":"https://i.pximg.net/cover.jpg","large":"https://i.pximg.net/p1"}}]}}`)
		return
	}
	_, _ = fmt.Fprint(w, `{"illust":{"id":123,"type":"illust","page_count":12,
        "image_urls":{"medium":"https://i.pximg.net/medium.png","large":"https://i.pximg.net/cover.jpg"}}}`)
}

func checkPixivMergedPreview(t *testing.T, previews *thumbnails, downloads *atomic.Int32) {
	t.Helper()
	small, large, actual, err := previews.generate(inlineLink("https://www.pixiv.net/artworks/124"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(large))
	if err != nil {
		t.Fatal(err)
	}
	checkCountBadge(t, previews, small, 2)
	checkCountBadge(t, previews, large, 2)
	checkMergeBadge(t, previews, small, 2, true)
	checkMergeBadge(t, previews, large, 2, true)
	if img.Bounds().Size() != image.Pt(400, 320) || actual != 1 || downloads.Load() != 3 {
		t.Fatalf("Pixiv merged preview: size %v, count %d, downloads %d", img.Bounds().Size(), actual, downloads.Load())
	}
}

func checkPixivPreviews(t *testing.T, client *http.Client, photoURL, thumbnailURL string) {
	t.Helper()
	res, err := client.Get(photoURL)
	if err != nil {
		t.Fatal(err)
	}
	large := readThumbnail(t, res)
	if large.Bounds().Size() != image.Pt(800, 1280) {
		t.Fatalf("portrait photo size: %v", large.Bounds().Size())
	}
	checkThumbnailEdges(t, large)
	res, err = client.Get(thumbnailURL)
	if err != nil {
		t.Fatal(err)
	}
	small := readThumbnail(t, res)
	if small.Bounds().Size() != image.Pt(240, 384) {
		t.Fatalf("portrait thumbnail size: %v", small.Bounds().Size())
	}
	checkThumbnailEdges(t, small)
	for _, img := range []image.Image{small, large} {
		size := img.Bounds().Size()
		side := min(size.X, size.Y)
		r, g, b, _ := img.At(size.X-74*side/384, size.Y-54*side/384).RGBA()
		if b <= r || g <= r {
			t.Fatal("Pixiv blue logo tile is missing")
		}
		r, g, b, _ = img.At(size.X-46*side/384, 20*side/384).RGBA()
		if r+g+b > 30000 {
			t.Fatal("Pixiv multi-page count badge is missing")
		}
	}
}

func TestMergedThumbnailSources(t *testing.T) {
	server, downloads, uploads := mergedThumbnailServer(t)
	defer server.Close()

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	db := testArtworkDB(t)
	previews := testPreviews(t, db, bot)
	target, _ := url.Parse(server.URL)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}

	link := inlineLink("https://x.com/user/status/10")
	work := previews.warmup(link, 0)
	<-work.done
	if work.err != nil {
		t.Fatal(work.err)
	}
	img, _, err := image.Decode(bytes.NewReader(work.large))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Size() != image.Pt(400, 320) || work.actual != 2 {
		t.Fatalf("joined preview: dimensions %v, count %d", img.Bounds().Size(), work.actual)
	}

	checkCountBadge(t, previews, work.data, 3)
	checkCountBadge(t, previews, work.large, 3)
	checkMergeBadge(t, previews, work.data, 3, true)
	checkMergeBadge(t, previews, work.large, 3, true)

	awaitCachedThumbnail(t, db, "x:10", 2)
	before := downloads.Load()
	id := previews.cached(link, 2)
	if id != "joined-preview" || downloads.Load() != before || uploads.Load() != 1 {
		t.Fatal("merged count did not reuse cached preview")
	}

	for _, tc := range []struct {
		id            string
		count, actual int
		size          image.Point
	}{
		{"11", 3, 3, image.Pt(240, 384)}, {"12", 3, 2, image.Pt(240, 384)}, {"14", 4, 3, image.Pt(240, 384)},
	} {
		link := inlineLink("https://x.com/user/status/" + tc.id)
		small, large, actual, err := previews.generate(link)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(large))
		if err != nil {
			t.Fatal(err)
		}
		checkCountBadge(t, previews, small, tc.count)
		checkCountBadge(t, previews, large, tc.count)
		checkMergeBadge(t, previews, small, tc.count, false)
		checkMergeBadge(t, previews, large, tc.count, false)
		if img.Bounds().Size() != tc.size || actual != tc.actual {
			t.Fatalf("%s: size %v count %d", tc.id, img.Bounds().Size(), actual)
		}
	}

	if _, _, _, err := previews.generate(inlineLink("https://x.com/user/status/13")); err == nil {
		t.Fatal("later failed download was ignored")
	}
}

func mergedThumbnailServer(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	encode := func(img image.Image) []byte {
		var data bytes.Buffer
		if err := png.Encode(&data, img); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	panels := [][]byte{
		encode(scene.SubImage(image.Rect(0, 0, 200, 320))),
		encode(scene.SubImage(image.Rect(200, 0, 400, 320))),
		encode(artScene(image.Rect(0, 0, 200, 320), 2)),
	}

	var server *httptest.Server
	downloads, uploads := &atomic.Int32{}, &atomic.Int32{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case "/bottoken/sendPhoto":
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
			} else {
				img, _, err := image.Decode(file)
				_ = file.Close()
				if err != nil {
					t.Error(err)
				} else if img.Bounds().Size() != image.Pt(400, 320) {
					t.Errorf("uploaded merged preview: %v", img.Bounds().Size())
				}
			}
			uploads.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"joined-preview"}]}}`)
		case "/bottoken/deleteMessage":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/p0", "/p1", "/p2":
			downloads.Add(1)
			_, _ = w.Write(panels[int(r.URL.Path[2]-'0')])
		case "/user/status/10", "/user/status/11", "/user/status/12", "/user/status/13", "/user/status/14":
			photo := func(n int) map[string]string {
				return map[string]string{"type": "photo", "url": fmt.Sprintf("%s/p%d", server.URL, n)}
			}
			video := map[string]string{"type": "video", "thumbnail_url": server.URL + "/p2"}
			var items []map[string]string
			switch r.URL.Path {
			case "/user/status/10":
				items = []map[string]string{photo(0), photo(1), photo(2)}
			case "/user/status/11":
				items = []map[string]string{photo(0), {"type": "video"}, photo(1)}
			case "/user/status/12":
				items = []map[string]string{video, photo(0), photo(1)}
			case "/user/status/13":
				items = []map[string]string{photo(0), {"type": "photo", "url": server.URL + "/missing"}}
			case "/user/status/14":
				items = []map[string]string{photo(2), {"type": "video"}, photo(0), photo(1)}
			}
			response := map[string]any{
				"code": 200, "tweet": map[string]any{"media": map[string]any{"all": items}},
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	return server, downloads, uploads
}

func checkMergeBadge(t *testing.T, previews *thumbnails, data []byte, count int, want bool) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(previews.font, &opentype.FaceOptions{Size: 22 * 4, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = face.Close() }()
	right := 370.0
	if count > 1 {
		right -= float64(font.MeasureString(face, fmt.Sprint(count)).Ceil())/4 + 47 + 8
	}
	left := right - 38
	size := img.Bounds().Size()
	scale := float64(min(size.X, size.Y)) / 384
	pixel := func(x, y float64) uint32 {
		r, g, b, _ := img.At(size.X-int((384-x)*scale), int(y*scale)).RGBA()
		return (r + g + b) / 3
	}
	dark := pixel(left+5, 34) < 18000
	strokes := 0
	for i := range 4 {
		bright := false
		for dx := -1.0; dx <= 1; dx++ {
			if pixel(left+10+float64(i*6)+dx/scale, 34) > 27000 {
				bright = true
			}
		}
		if bright {
			strokes++
		}
	}
	if got := dark && strokes == 4; got != want {
		t.Errorf("merge badge count=%d size=%v: dark=%v strokes=%d, want %v", count, size, dark, strokes, want)
	}
}

func checkCountBadge(t *testing.T, previews *thumbnails, data []byte, count int) {
	t.Helper()

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	alternative := count - 1
	if count == 2 {
		alternative = 3
	}
	checkBadgeLabel(t, previews, img, fmt.Sprint(count), fmt.Sprint(alternative))
}

func checkBadgeLabel(t *testing.T, previews *thumbnails, img image.Image, label, alternative string) int {
	t.Helper()
	badge := image.NewRGBA(image.Rect(0, 0, 384*4, 56*4))
	left, err := previews.badge(badge, label)
	if err != nil {
		t.Fatal(err)
	}
	other := image.NewRGBA(badge.Bounds())
	if _, err := previews.badge(other, alternative); err != nil {
		t.Fatal(err)
	}

	size := img.Bounds().Size()
	scale := float64(min(size.X, size.Y)) / 384
	// Compare only pixels where the expected and alternative numerals differ.
	matches, samples := 0, 0
	for y := 25; y < 42; y++ {
		for x := left/4 + 35; x < 366; x++ {
			r, g, b, _ := badge.At(x*4+2, y*4+2).RGBA()
			expected := (r + g + b) / 3
			r, g, b, _ = other.At(x*4+2, y*4+2).RGBA()
			other := (r + g + b) / 3
			if expected > 60000 && other < 10000 || expected < 10000 && other > 60000 {
				ir, ig, ib, _ := img.At(size.X-int((384-float64(x))*scale), int(float64(y)*scale)).RGBA()
				got := (ir + ig + ib) / 3
				if expected > 60000 && got > 30000 || expected < 10000 && got < 30000 {
					matches++
				}
				samples++
			}
		}
	}
	if samples == 0 || matches*100/samples < 55 {
		t.Errorf("count badge %s size %v: %d/%d distinguishing pixels", label, size, matches, samples)
	}
	return left
}

func TestThumbnailAspectRatios(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   image.Point
		merged bool
	}{
		{"landscape", image.Pt(640, 400), false}, {"portrait", image.Pt(400, 640), false},
		{"merged-landscape", image.Pt(640, 400), true}, {"merged-portrait", image.Pt(400, 640), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := aspectPanels(t, tc.size, tc.merged)

			uploaded := make(chan []byte, 1)
			server := aspectServer(t, parts, uploaded)
			defer server.Close()

			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			db := testArtworkDB(t)
			previews := testPreviews(t, db, bot)
			target, _ := url.Parse(server.URL)
			previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}

			link := inlineLink("https://x.com/user/status/20")
			work := previews.warmup(link, 0)
			<-work.done
			if work.err != nil {
				t.Fatal(work.err)
			}
			if work.actual != 1 {
				t.Fatalf("post-merge count = %d", work.actual)
			}

			var upload []byte
			select {
			case upload = <-uploaded:
			case <-time.After(5 * time.Second):
				t.Fatal("upload timed out")
			}

			checkAspectPreviews(t, previews, tc.size, tc.merged, [][]byte{work.data, work.large, upload})
			if !bytes.Equal(upload, work.large) {
				t.Error("cached upload differs from large preview")
			}

			awaitCachedThumbnail(t, db, "x:20", 1)
			if id := previews.cached(link, 1); id != "aspect" {
				t.Fatalf("post-merge cache alias = %q", id)
			}
			// A cached artwork can request a preview under its post-merge count before the upload is available.
			cachedWork := previews.start(link)
			<-cachedWork.done
			if cachedWork.err != nil {
				t.Fatal(cachedWork.err)
			}
			if !bytes.Equal(cachedWork.data, work.data) || !bytes.Equal(cachedWork.large, work.large) {
				t.Error("post-merge cache key changed preview")
			}
		})
	}
}

func aspectPanels(t *testing.T, size image.Point, merged bool) [][]byte {
	t.Helper()
	scene := artScene(image.Rectangle{Max: size}, 0)
	edge, _, err := image.Decode(bytes.NewReader(thumbnailFixture(t, size.X, size.Y)))
	if err != nil {
		t.Fatal(err)
	}
	for _, rect := range []image.Rectangle{
		image.Rect(0, 0, size.X/12, size.Y), image.Rect(size.X-size.X/12, 0, size.X, size.Y),
		image.Rect(0, 0, size.X, size.Y/12), image.Rect(0, size.Y-size.Y/12, size.X, size.Y),
	} {
		draw.Draw(scene, rect, edge, rect.Min, draw.Src)
	}
	var parts [][]byte
	rects := []image.Rectangle{scene.Bounds()}
	if merged {
		rects = []image.Rectangle{image.Rect(0, 0, size.X/2, size.Y), image.Rect(size.X/2, 0, size.X, size.Y)}
	}
	for _, rect := range rects {
		var b bytes.Buffer
		if err := png.Encode(&b, scene.SubImage(rect)); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, b.Bytes())
	}
	return parts
}

func aspectServer(t *testing.T, parts [][]byte, uploaded chan<- []byte) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottoken/getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case "/bottoken/sendPhoto":
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = file.Close() }()
			var b bytes.Buffer
			if _, err := b.ReadFrom(file); err != nil {
				t.Error(err)
				return
			}
			uploaded <- b.Bytes()
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"aspect"}]}}`)
		case "/bottoken/deleteMessage":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/user/status/20":
			items := make([]map[string]string, len(parts))
			for i := range parts {
				items[i] = map[string]string{"type": "photo", "url": fmt.Sprintf("%s/p%d", server.URL, i)}
			}
			response := map[string]any{"code": 200, "tweet": map[string]any{"media": map[string]any{"all": items}}}
			_ = json.NewEncoder(w).Encode(response)
		case "/p0", "/p1":
			_, _ = w.Write(parts[int(r.URL.Path[2]-'0')])
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func checkAspectPreviews(t *testing.T, previews *thumbnails, sourceSize image.Point, merged bool, data [][]byte) {
	t.Helper()
	for _, data := range data {
		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil || format != "jpeg" {
			t.Fatalf("image: %s %v", format, err)
		}
		size := img.Bounds().Size()
		longest := max(size.X, size.Y)
		if abs := float64(size.X)*float64(sourceSize.Y)/float64(sourceSize.X) - float64(size.Y); abs < -1 || abs > 1 {
			t.Errorf("aspect ratio %v, source %v", size, sourceSize)
		}
		if longest != 384 && longest != 640 {
			t.Errorf("unexpected dimensions %v", size)
		}
		checkThumbnailEdges(t, img)
		if merged {
			checkCountBadge(t, previews, data, 2)
			checkMergeBadge(t, previews, data, 2, true)
		}
	}
}

func TestPreviewDimensionsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE artwork (id TEXT PRIMARY KEY, url TEXT NOT NULL, author TEXT NOT NULL, text TEXT NOT NULL, caption TEXT NOT NULL, photos TEXT NOT NULL, media_types TEXT NOT NULL DEFAULT '[]');
	CREATE TABLE history (sequence INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, artwork_id TEXT NOT NULL, UNIQUE(user_id, artwork_id));
	CREATE TABLE thumbnail_cache (artwork_id TEXT NOT NULL, media_count INTEGER NOT NULL, file_id TEXT NOT NULL, render_version INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (artwork_id, media_count));
	INSERT INTO artwork VALUES ('x:1','https://x.com/user/status/1','','','','["media"]','["photo"]');
	INSERT INTO history(user_id,artwork_id) VALUES (1,'x:1');
	INSERT INTO thumbnail_cache VALUES ('x:1',1,'existing',?)`, thumbnailRenderVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	a, err := findArtwork(db, "x:1")
	if err != nil || a.PreviewWidth != 0 || a.PreviewHeight != 0 {
		t.Fatalf("migrated: %+v, %v", a, err)
	}
	if id, err := findThumbnail(db, "x:1", 1); err != nil || id != "existing" {
		t.Fatalf("cache: %q, %v", id, err)
	}

	if err := savePreviewDimensions(db, "x:1", 1280, 800); err != nil {
		t.Fatal(err)
	}
	if err := saveSelection(db, 1, *a); err != nil {
		t.Fatal(err)
	}
	a, err = findArtwork(db, "x:1")
	if err != nil || a.PreviewWidth != 1280 || a.PreviewHeight != 800 {
		t.Fatalf("upsert lost dimensions: %+v, %v", a, err)
	}
}

func TestThumbnailXQuotedMedia(t *testing.T) {
	cases := []struct {
		name, tweet string
		want        thumbnailMedia
	}{
		{"quote photo", `{"media":{"all":[]},"quote":{"media":{"all":[{"type":"photo","url":"https://media/quote","thumbnail_url":"https://media/preview"}]}}}`, thumbnailMedia{Type: "photo", URL: "https://media/quote", ThumbnailURL: "https://media/preview"}},
		{"quote video", `{"quote":{"media":{"all":[{"type":"video","url":"https://media/video","thumbnail_url":"https://media/preview"}]}}}`, thumbnailMedia{Type: "video", URL: "https://media/video", ThumbnailURL: "https://media/preview"}},
		{"outer", `{"media":{"all":[{"type":"photo","url":"https://media/outer"}]},"quote":{"media":{"all":[{"type":"photo","url":"https://media/quote"}]}}}`, thumbnailMedia{Type: "photo", URL: "https://media/outer"}},
		{"no media", `{}`, thumbnailMedia{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: xPostTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200, Status: "OK",
					Body:   io.NopCloser(strings.NewReader(`{"code":200,"tweet":` + tc.tweet + `}`)),
					Header: make(http.Header), Request: r,
				}, nil
			})}
			link, _ := url.Parse("https://x.com/user/status/123")
			source, err := (&thumbnails{client: client}).xSource(link)
			if tc.want.URL == "" {
				if err == nil {
					t.Fatal("expected missing preview error")
				}
				return
			}
			if err != nil || source.count != 1 || len(source.media) != 1 || source.media[0] != tc.want {
				t.Fatalf("source = %+v, error = %v", source, err)
			}
		})
	}
}
