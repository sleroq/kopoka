package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPostImagePreviewFlow(t *testing.T) { testPostImagePreviewFlow(t, "") }
func TestPostImagePreviewFlowRust(t *testing.T) {
	testPostImagePreviewFlow(t, testPostImageRenderer(t))
}

func testPostImagePreviewFlow(t *testing.T, renderer string) {
	t.Helper()

	dir := t.TempDir()
	var imageData bytes.Buffer
	if err := jpeg.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 800, 400)), nil); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "image.jpg")
	if err := os.WriteFile(imagePath, imageData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREVIEW_TEST_DIR", dir)
	t.Setenv("PREVIEW_TEST_IMAGE", imagePath)
	t.Setenv("PREVIEW_TEST_RENDERER", renderer)
	script := `#!/bin/sh
input=$(cat)
case "$input" in *'"preview":'*)
  if [ -n "$PREVIEW_TEST_RENDERER" ]; then printf '%s' "$input" | "$PREVIEW_TEST_RENDERER"; else cat "$PREVIEW_TEST_IMAGE"; fi
  exit;; esac
case "$input" in *'"theme":"light"'*) theme=light;; *) theme=dark;; esac
case "$input" in *'"carousel":true'*) theme="$theme-bars";; esac
printf '%s' "$input" > "$PREVIEW_TEST_DIR/$theme.json"
while [ ! -f "$PREVIEW_TEST_DIR/light.json" ] || [ ! -f "$PREVIEW_TEST_DIR/dark.json" ] || [ ! -f "$PREVIEW_TEST_DIR/light-bars.json" ] || [ ! -f "$PREVIEW_TEST_DIR/dark-bars.json" ]; do sleep 0.01; done
if [ -n "$PREVIEW_TEST_RENDERER" ]; then exec "$PREVIEW_TEST_RENDERER" < "$PREVIEW_TEST_DIR/$theme.json"; fi
cat "$PREVIEW_TEST_IMAGE"
`
	path := filepath.Join(dir, "renderer")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", path)

	var sources, assets, uploads, edits atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case (r.URL.Path == "/user/status/42" || r.URL.Path == "/user/status/42/en"):
			sources.Add(1)
			asset := server.URL + "/asset"
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
				"text": "hello", "author": map[string]string{"name": "Artist", "screen_name": "user", "avatar_url": asset},
				"media": map[string]any{
					"photos": []map[string]any{{"url": asset, "formats": []any{}}, {"url": asset, "formats": []any{}}},
					"all":    []map[string]any{{"url": asset, "type": "photo", "formats": []any{}}, {"url": asset, "type": "photo", "formats": []any{}}},
				},
			}})
		case r.URL.Path == "/asset":
			assets.Add(1)
			_, _ = w.Write(imageData.Bytes())
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			uploads.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"card"}]}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
			edits.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	db := testArtworkDB(t)
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews := testPreviews(t, db, bot)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/user/status/42/photo/2?foo=bar")

	for _, theme := range []string{"light", "dark", "light-bars", "dark-bars"} {
		result := postImageResult(db, link, theme, defaultTimezone, defaultTranslationSettings(), previews, nil)
		if !strings.Contains(result["photo_url"].(string), "placeholder-"+strings.TrimSuffix(theme, "-bars")) {
			t.Fatalf("instant: %v", result)
		}
	}
	if sources.Load() != 0 || len(previews.postImageWork) != 0 {
		t.Fatal("instant preview started work")
	}

	work := previews.startPostImagePreview(link, defaultTimezone, defaultTranslationSettings(), "light", "dark")
	results := make(chan map[string]any, 4)
	for _, theme := range []string{"light", "dark", "light-bars", "dark-bars"} {
		go func(theme string) {
			results <- postImageResult(db, link, theme, defaultTimezone, defaultTranslationSettings(), previews, work)
		}(theme)
	}
	checkPostImagePreviewResponses(t, previews, results)

	if sources.Load() != 1 || assets.Load() != 1 {
		t.Fatalf("sources=%d assets=%d", sources.Load(), assets.Load())
	}

	t.Setenv("KOPOKA_RENDERER", filepath.Join(dir, "missing-renderer"))
	for _, theme := range []string{"light", "dark", "light-bars", "dark-bars"} {
		data, err := os.ReadFile(filepath.Join(dir, theme+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var request renderRequest
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Theme != strings.TrimSuffix(theme, "-bars") || request.Post.Media.Carousel != strings.HasSuffix(theme, "-bars") || request.Post.Media.Bars != strings.HasSuffix(theme, "-bars") || request.Post.Text != "hello" || request.Post.AuthorName != "Artist" || len(request.Images) != 1 {
			t.Fatalf("request: %+v", request)
		}
		chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1}, InlineMessageID: "inline"}
		if err := editInlinePostImage(bot, db, chosen, "@cache", link, theme, previews, nil); err != nil {
			t.Fatal(err)
		}
		result := postImageResult(db, link, theme, defaultTimezone, defaultTranslationSettings(), previews, work)
		if result["id"] != postImageID(link, theme)+":cached" || result["input_message_content"].(map[string]any)["message_text"] != "Loading post image…" || result["photo_url"] == nil {
			t.Fatalf("cached: %v", result)
		}
	}
	if sources.Load() != 1 || assets.Load() != 1 || uploads.Load() != 4 || edits.Load() != 4 {
		t.Fatalf("sources=%d assets=%d uploads=%d edits=%d", sources.Load(), assets.Load(), uploads.Load(), edits.Load())
	}
}

func checkPostImagePreviewResponses(t *testing.T, previews *thumbnails, results <-chan map[string]any) {
	t.Helper()
	for range 4 {
		result := <-results
		address, err := url.Parse(result["photo_url"].(string))
		if err != nil || address.Path != "/post-image-preview" || address.Query().Get("url") != "https://x.com/user/status/42" {
			t.Fatalf("preview URL: %v", result)
		}
		theme := strings.Split(result["id"].(string), ":")[1]
		if address.Query().Get("theme") != theme || address.Query().Get("tz") != defaultTimezone || address.Query().Get("v") != fmt.Sprint(postImageVersion) {
			t.Fatalf("preview variant: %v", result)
		}
		for _, size := range []string{"large", ""} {
			q := address.Query()
			if size == "" {
				q.Del("size")
			}
			address.RawQuery = q.Encode()
			response := httptest.NewRecorder()
			previews.ServeHTTP(response, httptest.NewRequest("GET", address.RequestURI(), nil))
			img, err := jpeg.Decode(bytes.NewReader(response.Body.Bytes()))
			if err != nil || response.Code != 200 {
				t.Fatalf("preview response: %d %v", response.Code, err)
			}
			bounds := img.Bounds().Size()
			if size == "large" && (result["photo_width"] != bounds.X || result["photo_height"] != bounds.Y) {
				t.Fatalf("dimensions: %v %v", bounds, result)
			}
			if size == "" && max(bounds.X, bounds.Y) != 384 {
				t.Fatalf("thumbnail dimensions: %v", bounds)
			}
		}
	}
}

func TestPostImagePreviewRetryAfterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "renderer")
	imagePath := filepath.Join(dir, "image.jpg")
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 40, 20)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat >/dev/null\n[ -f '" + dir + "/ready' ] || exit 1\ncat '" + imagePath + "'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", path)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"author":{"name":"Artist","screen_name":"user"}}}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	previews := testPreviews(t, testArtworkDB(t), nil)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/user/status/42")
	work := previews.startPostImagePreview(link, defaultTimezone, defaultTranslationSettings(), "light", "dark")
	<-work.variants["light"].done
	<-work.variants["dark"].done
	if work.variants["light"].err == nil || work.variants["dark"].err == nil {
		t.Fatal("expected render failure")
	}

	deadline := time.After(2 * time.Second)
	for {
		previews.mu.Lock()
		finished := !work.finished.IsZero()
		previews.mu.Unlock()
		if finished {
			break
		}
		select {
		case <-deadline:
			t.Fatal("failed work did not finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	for _, theme := range []string{"light", "dark", "light-bars", "dark-bars"} {
		result := postImageResult(previews.db, link, theme, defaultTimezone, defaultTranslationSettings(), previews, work)
		if !strings.Contains(result["photo_url"].(string), "placeholder-") {
			t.Fatalf("failed preview: %v", result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("failed work was requested again: %d", calls.Load())
	}

	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	retry := previews.startPostImagePreview(link, defaultTimezone, defaultTranslationSettings(), "light")
	if photo := previews.postImagePreview(link, "light", defaultTimezone, defaultTranslationSettings(), retry); !strings.Contains(photo.photoURL, "/post-image-preview?") {
		t.Fatalf("retry: %+v", photo)
	}
	if calls.Load() != 2 {
		t.Fatalf("metadata requests: %d", calls.Load())
	}
}

func TestPostImagePreviewSelectedThemes(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"Hello","author":{"name":"Artist","screen_name":"user"}}}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	previews := testPreviews(t, testArtworkDB(t), nil)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/user/status/42")
	work := previews.startPostImagePreview(link, defaultTimezone, defaultTranslationSettings(), "warm", "dim")
	for _, theme := range []string{"warm", "dim", "warm-bars", "dim-bars"} {
		photo := previews.postImagePreview(link, theme, defaultTimezone, defaultTranslationSettings(), work)
		address, err := url.Parse(photo.photoURL)
		if err != nil || address.Query().Get("theme") != theme {
			t.Fatalf("%s preview: %+v %v", theme, photo, err)
		}
		response := httptest.NewRecorder()
		previews.ServeHTTP(response, httptest.NewRequest("GET", address.RequestURI(), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s preview: %d", theme, response.Code)
		}
	}
	previews.mu.Lock()
	variants := len(work.variants)
	previews.mu.Unlock()
	if calls.Load() != 1 || variants != 4 {
		t.Fatalf("fetches=%d variants=%d", calls.Load(), variants)
	}
}

func TestPostImagePreviewTimezones(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "tweet": map[string]any{
				"created_timestamp": 1704065400,
				"author":            map[string]string{"name": "Artist", "screen_name": "user"},
			},
		})
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	previews := testPreviews(t, testArtworkDB(t), nil)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/user/status/42")
	alias := inlineLink("https://twitter.com/other/status/42/photo/2")
	first := previews.startPostImagePreview(link, defaultTimezone, defaultTranslationSettings(), "light", "dark")
	if first != previews.startPostImagePreview(alias, defaultTimezone, defaultTranslationSettings(), "light", "dark") {
		t.Fatal("same post and zone did not share work")
	}
	if first == previews.startPostImagePreview(link, "UTC", defaultTranslationSettings(), "light", "dark") {
		t.Fatal("different zones shared work")
	}
	var images [][]byte
	for _, zone := range []string{defaultTimezone, "UTC"} {
		work := previews.startPostImagePreview(link, zone, defaultTranslationSettings(), "light")
		photo := previews.postImagePreview(link, "light", zone, defaultTranslationSettings(), work)
		address, err := url.Parse(photo.photoURL)
		if err != nil || address.Query().Get("tz") != zone {
			t.Fatalf("URL %q: %v", photo.photoURL, err)
		}
		response := httptest.NewRecorder()
		previews.ServeHTTP(response, httptest.NewRequest("GET", address.RequestURI(), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("preview: %d", response.Code)
		}
		images = append(images, response.Body.Bytes())
	}
	if bytes.Equal(images[0], images[1]) || calls.Load() != 2 {
		t.Fatalf("images equal=%v fetches=%d", bytes.Equal(images[0], images[1]), calls.Load())
	}
}
