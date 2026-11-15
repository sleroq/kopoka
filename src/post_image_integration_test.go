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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPostImageInlineFlow(t *testing.T) {
	testPostImageInlineFlow(t, "")
}

func TestPostImageInlineFlowRust(t *testing.T) {
	testPostImageInlineFlow(t, testPostImageRenderer(t))
}

func testPostImageRenderer(t *testing.T) string {
	t.Helper()
	if renderer := os.Getenv("KOPOKA_RENDERER"); renderer != "" {
		return renderer
	}
	if renderer, err := exec.LookPath("kopoka-render"); err == nil {
		return renderer
	}
	renderer, err := filepath.Abs(filepath.Join("..", "target", "release", "kopoka-render"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(renderer); err != nil {
		t.Skip("build Rust renderer with cargo build --release or set KOPOKA_RENDERER")
	}
	return renderer
}

func testPostImageInlineFlow(t *testing.T, realRenderer string) {
	t.Helper()
	db := testArtworkDB(t)
	if _, err := db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}

	var jpg bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	img.Set(0, 0, color.White)
	if err := jpeg.Encode(&jpg, img, nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "post.jpg")
	if err := os.WriteFile(imagePath, jpg.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	renderer := filepath.Join(dir, "renderer")
	t.Setenv("KOPOKA_TEST_REQUEST", filepath.Join(dir, "request.json"))
	t.Setenv("KOPOKA_TEST_IMAGE", imagePath)
	script := "#!/bin/sh\ncat > \"$KOPOKA_TEST_REQUEST\"\ncat \"$KOPOKA_TEST_IMAGE\"\n"
	if realRenderer != "" {
		t.Setenv("KOPOKA_TEST_RENDERER", realRenderer)
		script = "#!/bin/sh\ncat > \"$KOPOKA_TEST_REQUEST\"\nexec \"$KOPOKA_TEST_RENDERER\" < \"$KOPOKA_TEST_REQUEST\"\n"
	}
	if err := os.WriteFile(renderer, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", renderer)
	var calls []string
	var results []map[string]any
	server := postImageTelegramServer(t, jpg.Bytes(), &calls, &results)
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews := testPreviews(t, db, bot)
	query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/42/photo/2"}
	handleInlineQuery(bot, db, query, previews)
	if len(results) != 3 || results[1]["id"] != "post-image:light:42" || results[2]["id"] != "post-image:dark:42" || strings.Join(calls, ",") != "answer" {
		t.Fatalf("results %v calls %v", results, calls)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	for _, theme := range []string{"light", "dark"} {
		chosen := &tgbotapi.ChosenInlineResult{
			From: query.From, ResultID: "post-image:" + theme + ":42",
			InlineMessageID: "inline", Query: query.Query,
		}
		handleChosenInlineResult(bot, nil, db, chosen, "@cache", queue, previews)
		var request renderRequest
		data, err := os.ReadFile(filepath.Join(dir, "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Theme != theme || request.Post.Quote == nil || request.Post.Quote.Text != "quote" || len(request.Post.Media.Photos) != 2 || len(request.Images) != 2 {
			t.Fatalf("request: %+v", request)
		}
	}
	before := len(calls)
	handleInlineQuery(bot, db, query, previews)
	if len(calls) != before+1 || results[1]["photo_file_id"] != "card" || results[2]["photo_file_id"] != "card" || !strings.Contains(results[1]["caption"].(string), "/status/42") {
		t.Fatalf("cached results %v calls %v", results, calls)
	}
	before = len(calls)
	handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
		From: query.From, ResultID: results[2]["id"].(string), Query: query.Query,
	}, "@cache", queue, previews)
	if len(calls) != before {
		t.Fatalf("cached selection made requests: %v", calls[before:])
	}
	if n, err := historyCount(db, 1); err != nil || n != 0 {
		t.Fatalf("history: %d %v", n, err)
	}
}

func postImageTelegramServer(t *testing.T, jpg []byte, calls *[]string, results *[]map[string]any) *httptest.Server {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user/status/42":
			*calls = append(*calls, "fetch")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
				"text": "hello", "author": map[string]string{"name": "Artist", "screen_name": "user", "avatar_url": server.URL + "/asset"},
				"media": map[string]any{"photos": []postImage{{URL: server.URL + "/asset"}, {URL: server.URL + "/asset2"}}},
				"quote": map[string]any{"text": "quote", "author": map[string]string{"name": "Quoted", "screen_name": "quoted"}},
			}})
		case strings.HasPrefix(r.URL.Path, "/asset"):
			*calls = append(*calls, "asset")
			_, _ = w.Write(jpg)
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			*calls = append(*calls, "answer")
			_ = r.ParseForm()
			if err := json.Unmarshal([]byte(r.FormValue("results")), results); err != nil {
				t.Error(err)
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			*calls = append(*calls, "upload")
			_ = r.ParseMultipartForm(1 << 20)
			f, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
			} else {
				_, err = jpeg.Decode(f)
				if err != nil {
					t.Error(err)
				}
				_ = f.Close()
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"card"}]}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
			*calls = append(*calls, "edit")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	return server
}

func TestPostImageRendererTextOnly(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			request := renderRequest{Theme: theme, Post: renderPost{
				AuthorName: "Artist", AuthorUsername: "artist", Text: "A text-only post",
			}}
			data, err := renderPostImage(request)
			if err != nil {
				t.Fatal(err)
			}
			picture, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			red, _, _, _ := picture.At(12, 12).RGBA()
			if theme == "light" && red < 60000 || theme == "dark" && red > 20000 {
				t.Fatalf("%s card background = %d", theme, red)
			}
		})
	}
}

func TestPostImageRendererFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "renderer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho renderer-failure >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", path)
	_, err := renderPostImage(renderRequest{Theme: "light"})
	if err == nil || !strings.Contains(err.Error(), "renderer-failure") {
		t.Fatalf("renderer error: %v", err)
	}
}

func TestPostImageReplyChainAndCard(t *testing.T) {
	var paths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/cover" {
			var data bytes.Buffer
			_ = jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil)
			_, _ = w.Write(data.Bytes())
			return
		}
		id := strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/"):], "/")
		parents := map[string]string{"5": "4", "4": "3", "3": "2", "2": "1"}
		tweet := map[string]any{
			"text": "text " + id, "author": map[string]string{"name": "User", "screen_name": "user"},
			"replying_to_status": parents[id],
		}
		if id == "5" {
			tweet["text"] = "link https://example.com/article  "
			tweet["card"] = map[string]any{
				"title": "Article", "url": "https://example.com/article",
				"image": map[string]string{"url": "http://" + r.Host + "/cover"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	link, _ := url.Parse("https://x.com/user/status/5/photo/2")
	request, err := preparePostImage(link, "light")
	if err != nil {
		t.Fatal(err)
	}
	if len(request.ReplyChain) != 3 || !request.HasMore || request.ReplyChain[0].Text != "text 2" || request.ReplyChain[2].Text != "text 4" {
		t.Fatalf("chain: %+v", request)
	}
	if request.Post.Text != "link" || request.Post.Card == nil || request.Post.Card.Domain != "example.com" || len(request.Images[request.Post.Card.Image.URL]) == 0 {
		t.Fatalf("post: %+v", request.Post)
	}
	if strings.Join(paths, ",") != "/user/status/5,/i/status/4,/i/status/3,/i/status/2,/cover" {
		t.Fatalf("paths: %v", paths)
	}
}

func TestPostImageOptionalCardHTTP(t *testing.T) {
	for _, title := range []string{"Text-only card", ""} {
		t.Run(title, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
					"author": map[string]string{"name": "Author", "screen_name": "author"},
					"text":   "Post https://example.com/page",
					"card":   map[string]string{"url": "https://example.com/page", "title": title},
				}})
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			original := http.DefaultTransport
			http.DefaultTransport = redirectTransport{target, original}
			defer func() { http.DefaultTransport = original }()

			request, err := preparePostImage(inlineLink("https://x.com/author/status/7"), "light")
			if err != nil {
				t.Fatal(err)
			}
			if title == "" {
				if request.Post.Card != nil || request.Post.Text != "Post https://example.com/page" {
					t.Fatalf("incomplete card changed post: %+v", request.Post)
				}
			} else if request.Post.Card == nil || request.Post.Card.Image != nil || request.Post.Text != "Post" {
				t.Fatalf("text-only card: %+v", request.Post)
			}
		})
	}
}
