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
	if _, err := db.Exec(`UPDATE thumbnail_cache SET render_version=1 WHERE artwork_id='x:1'`); err != nil {
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
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("inline query waited for the source")
	}
	photoURL, thumbnailURL := uncachedThumbnailURLs(t, (<-s.answers)[0])
	select {
	case <-s.started:
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail generation did not start")
	}
	localURL := previewServer.URL + strings.TrimPrefix(thumbnailURL, "https://preview.example.com")
	largeURL := previewServer.URL + strings.TrimPrefix(photoURL, "https://preview.example.com")
	images := awaitThumbnails(t, []string{localURL, largeURL}, func() { s.releaseOnce.Do(func() { close(s.release) }) })
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
	if err != nil || version != 2 {
		t.Fatalf("cached render version: %d, %v", version, err)
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
	if s.upstream.Load() != 1 || s.downloads.Load() != 1 {
		t.Fatalf("metadata requests = %d, image downloads = %d, want one each", s.upstream.Load(), s.downloads.Load())
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
	if pq.Get("size") != "large" || tq.Has("size") || tq.Get("url") == "" {
		t.Fatalf("preview queries: %v, %v", pq, tq)
	}
	pq.Del("size")
	if !reflect.DeepEqual(pq, tq) {
		t.Fatalf("preview queries differ: %v, %v", pq, tq)
	}
	if _, width := result["photo_width"]; width {
		t.Fatalf("inline result fixes an unknown aspect ratio: %#v", result)
	}
	if _, height := result["photo_height"]; height {
		t.Fatalf("inline result fixes an unknown aspect ratio: %#v", result)
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
	preview := strings.TrimPrefix(result["photo_url"].(string), "https://preview.example.com")
	res, err := http.Get(server.URL + preview)
	if err != nil {
		t.Fatal(err)
	}
	_ = readThumbnail(t, res)
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

func awaitThumbnails(t *testing.T, endpoints []string, release func()) []image.Image {
	t.Helper()
	type response struct {
		index int
		res   *http.Response
		err   error
	}
	results := make(chan response, len(endpoints))
	for i, endpoint := range endpoints {
		go func() { res, err := http.Get(endpoint); results <- response{i, res, err} }()
	}
	select {
	case result := <-results:
		if result.res != nil {
			_ = result.res.Body.Close()
		}
		t.Fatalf("preview returned before source was ready: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	images := make([]image.Image, len(endpoints))
	for range endpoints {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			images[result.index] = readThumbnail(t, result.res)
		case <-time.After(5 * time.Second):
			t.Fatal("preview did not finish")
		}
	}
	return images
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
	handleInlineQuery(bot, db, query, previews)
	cached := (<-answers)[0]
	rich := cached["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	media := rich["media"].([]any)[0].(map[string]any)["media"].(map[string]any)
	if cached["type"] != "photo" || cached["id"] != "x:1" || media["type"] != "video" || media["media"] != "video-id" || !strings.Contains(rich["html"].(string), "tg://video?id=0") {
		t.Fatalf("cached single result: %#v", cached)
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
	// The count badge must appear only on the multi-image preview.
	size := single.Bounds().Size()
	side := min(size.X, size.Y)
	badgeX, badgeY := size.X-46*side/384, 20*side/384
	mr, mg, mb, _ := multi.At(badgeX, badgeY).RGBA()
	sr, sg, sb, _ := single.At(badgeX, badgeY).RGBA()
	if mr+mg+mb >= sr+sg+sb {
		t.Fatal("multi-image preview is missing the count badge")
	}
	// The black X tile sits inside rounded corners over the artwork.
	r, g, b, _ := single.At(size.X-74*side/384, size.Y-54*side/384).RGBA()
	if r+g+b > 10000 {
		t.Fatal("X logo tile is missing")
	}
	r, g, b, _ = single.At(size.X-87*side/384, size.Y-87*side/384).RGBA()
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
			_, _ = fmt.Fprint(w, `{"illust":{"id":123,"type":"illust","page_count":12,
                "image_urls":{"medium":"https://i.pximg.net/medium.png","large":"https://i.pximg.net/cover.jpg"}}}`)
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
	_, photoURL, thumbnailURL := previews.photo(link, 0)
	checkPixivPreviews(t, server.Client(), photoURL, thumbnailURL)
	awaitCachedThumbnail(t, db, "pixiv:123", 0)
	awaitCachedThumbnail(t, db, "pixiv:123", 12)
	if downloads.Load() != 1 || metadata.Load() != 1 {
		t.Fatalf("Pixiv metadata requests: %d, image downloads: %d", metadata.Load(), downloads.Load())
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
