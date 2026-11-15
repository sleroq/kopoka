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
case "$input" in *'"theme":"light"'*) theme=light;; *) theme=dark;; esac
printf '%s' "$input" > "$PREVIEW_TEST_DIR/$theme.json"
while [ ! -f "$PREVIEW_TEST_DIR/light.json" ] || [ ! -f "$PREVIEW_TEST_DIR/dark.json" ]; do sleep 0.01; done
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
		case r.URL.Path == "/user/status/42":
			sources.Add(1)
			response := `{"code":200,"tweet":{"text":"hello","author":{"name":"Artist","screen_name":"user","avatar_url":%q}}}`
			_, _ = fmt.Fprintf(w, response, server.URL+"/asset")
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

	for _, theme := range []string{"light", "dark"} {
		result := postImageResult(db, link, theme, previews, true)
		if !strings.Contains(result["photo_url"].(string), "placeholder-"+theme) {
			t.Fatalf("instant: %v", result)
		}
	}
	if sources.Load() != 0 || len(previews.postImageWork) != 0 {
		t.Fatal("instant preview started work")
	}

	results := make(chan map[string]any, 2)
	for _, theme := range []string{"light", "dark"} {
		go func(theme string) { results <- postImageResult(db, link, theme, previews, false) }(theme)
	}
	checkPostImagePreviewResponses(t, previews, results)

	if sources.Load() != 1 || assets.Load() != 1 {
		t.Fatalf("sources=%d assets=%d", sources.Load(), assets.Load())
	}

	t.Setenv("KOPOKA_RENDERER", filepath.Join(dir, "missing-renderer"))
	for _, theme := range []string{"light", "dark"} {
		data, err := os.ReadFile(filepath.Join(dir, theme+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var request renderRequest
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Theme != theme || request.Post.Text != "hello" || request.Post.AuthorName != "Artist" || len(request.Images) != 1 {
			t.Fatalf("request: %+v", request)
		}
		chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1}, InlineMessageID: "inline"}
		if err := editInlinePostImage(bot, db, chosen, "@cache", link, theme, previews); err != nil {
			t.Fatal(err)
		}
		result := postImageResult(db, link, theme, previews, false)
		if result["photo_file_id"] != "card" || result["id"] != postImageID(link, theme)+":cached" || !strings.Contains(result["caption"].(string), "Artist") {
			t.Fatalf("cached: %v", result)
		}
	}
	if sources.Load() != 1 || assets.Load() != 1 || uploads.Load() != 2 || edits.Load() != 2 {
		t.Fatalf("sources=%d assets=%d uploads=%d edits=%d", sources.Load(), assets.Load(), uploads.Load(), edits.Load())
	}
}

func checkPostImagePreviewResponses(t *testing.T, previews *thumbnails, results <-chan map[string]any) {
	t.Helper()
	for range 2 {
		result := <-results
		address, err := url.Parse(result["photo_url"].(string))
		if err != nil || address.Path != "/post-image-preview" || address.Query().Get("url") != "https://x.com/user/status/42" || address.Query().Get("v") != fmt.Sprint(postImageVersion) {
			t.Fatalf("preview URL: %v", result)
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
	work := previews.startPostImagePreview(link)
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

	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if photo := previews.postImagePreview(link, "light", false); !strings.Contains(photo.photoURL, "/post-image-preview?") {
		t.Fatalf("retry: %+v", photo)
	}
	if calls.Load() != 2 {
		t.Fatalf("metadata requests: %d", calls.Load())
	}
}
