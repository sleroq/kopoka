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
	"path/filepath"
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
	upstream, failures, uploads                           atomic.Int32
	answers                                               chan []map[string]any
}

func newThumbnailTestServer(t *testing.T) *thumbnailTestServer {
	t.Helper()
	s := &thumbnailTestServer{
		t: t, fixture: thumbnailFixture(t, 480, 300), answers: make(chan []map[string]any, 10),
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
			"photos": []any{map[string]any{"type": "photo", "url": s.server.URL + "/cover.png"}},
		}}})
	case r.URL.Path == "/cover.png":
		_, _ = w.Write(s.fixture)
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
	thumbnailURL := uncachedThumbnailURL(t, (<-s.answers)[0])
	select {
	case <-s.started:
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail generation did not start")
	}
	localURL := previewServer.URL + strings.TrimPrefix(thumbnailURL, "https://preview.example.com")
	multi := awaitThumbnail(t, localURL, func() { s.releaseOnce.Do(func() { close(s.release) }) })
	if multi.Bounds().Size() != image.Pt(384, 240) {
		t.Fatalf("landscape thumbnail size: %v", multi.Bounds().Size())
	}
	checkThumbnailEdges(t, multi)
	select {
	case <-s.uploadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail upload did not start")
	}
	s.uploadReleaseOnce.Do(func() { close(s.uploadRelease) })
	awaitCachedThumbnail(t, db, "x:1", 0)
	awaitCachedThumbnail(t, db, "x:1", 2)
	before := s.upstream.Load()
	handleInlineQuery(bot, db, query, previews)
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" {
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
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" {
		t.Fatalf("reopened thumbnail: %#v", cached)
	}
	query.Query = "https://vxtwitter.com/other/status/1"
	handleInlineQuery(bot, db, query, previews)
	if cached := (<-s.answers)[0]; cached["photo_file_id"] != "thumbnail-2" {
		t.Fatalf("thumbnail via alternate host: %#v", cached)
	}
	query.Query = "https://x.com/user/status/1"
	if s.upstream.Load() != 1 {
		t.Fatalf("source requests = %d, want shared generation", s.upstream.Load())
	}

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

func uncachedThumbnailURL(t *testing.T, result map[string]any) string {
	t.Helper()
	thumbnailURL, ok := result["thumbnail_url"].(string)
	if !ok || result["type"] != "photo" || result["photo_url"] != thumbnailURL || !strings.HasPrefix(thumbnailURL, "https://preview.example.com/thumbnail?") {
		t.Fatalf("inline result: %#v", result)
	}
	if _, width := result["photo_width"]; width {
		t.Fatalf("inline result fixes an unknown aspect ratio: %#v", result)
	}
	if _, height := result["photo_height"]; height {
		t.Fatalf("inline result fixes an unknown aspect ratio: %#v", result)
	}
	return thumbnailURL
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
	if result := (<-answers)[0]; result["photo_url"] != "https://preview.example.com/thumbnail-placeholder-x.jpg" || upstream.Load() != before {
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
	_ = readThumbnail(t, res)
	time.Sleep(100 * time.Millisecond)
	id, err := findThumbnail(db, "x:2", 5)
	if err != nil || id != "" || uploads.Load() != before {
		t.Fatalf("direct request cached a thumbnail: id %q, uploads %d, error %v", id, uploads.Load()-before, err)
	}
}

func awaitThumbnail(t *testing.T, endpoint string, release func()) image.Image {
	t.Helper()
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		res, err := http.Get(endpoint)
		if err != nil {
			requestErr <- err
			return
		}
		response <- res
	}()
	select {
	case res := <-response:
		_ = res.Body.Close()
		t.Fatal("thumbnail returned before source was ready")
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case res := <-response:
		return readThumbnail(t, res)
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("thumbnail did not finish")
	}
	return nil
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
	size := img.Bounds().Size()
	if max(size.X, size.Y) != 384 {
		t.Fatalf("thumbnail size: %v", size)
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

func TestPixivThumbnailFlow(t *testing.T) {
	fixture := thumbnailFixture(t, 300, 480)
	var previews *thumbnails
	var downloads atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/thumbnail":
			previews.ServeHTTP(w, r)
		case "/bottoken/getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case "/bottoken/sendPhoto":
			_ = r.ParseMultipartForm(8 << 20)
			if r.FormValue("chat_id") != "@cache" {
				t.Errorf("cache chat: %q", r.FormValue("chat_id"))
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"pixiv-thumbnail"}]}}`)
		case "/bottoken/deleteMessage":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/auth/token":
			_, _ = fmt.Fprint(w, `{"response":{"access_token":"access","refresh_token":"refresh","expires_in":3600,"user":{}}}`)
		case "/v1/illust/detail":
			_, _ = fmt.Fprint(w, `{"illust":{"id":123,"type":"illust","page_count":12,
                "image_urls":{"medium":"https://i.pximg.net/cover.png"}}}`)
		case "/cover.png":
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
	_, photoURL := previews.photo(link, 0)
	res, err := server.Client().Get(photoURL)
	if err != nil {
		t.Fatal(err)
	}
	img := readThumbnail(t, res)
	if img.Bounds().Size() != image.Pt(240, 384) {
		t.Fatalf("portrait thumbnail size: %v", img.Bounds().Size())
	}
	checkThumbnailEdges(t, img)
	awaitCachedThumbnail(t, db, "pixiv:123", 0)
	awaitCachedThumbnail(t, db, "pixiv:123", 12)
	if downloads.Load() != 1 {
		t.Fatalf("Pixiv image downloads: %d", downloads.Load())
	}
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
