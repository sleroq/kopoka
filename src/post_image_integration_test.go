package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

func TestPostImageRendererCarouselArrow(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))

	for _, tc := range []struct {
		name    string
		width   int
		compact bool
		arrow   bool
	}{
		{"fits", 20, false, false},
		{"fits-noncompact-viewport", 25, false, false},
		{"overflows-compact-viewport", 25, true, true},
		{"overflows", 30, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, tc.width, 100))
			draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
			var asset bytes.Buffer
			if err := png.Encode(&asset, img); err != nil {
				t.Fatal(err)
			}
			post := renderPost{AuthorName: "Artist", AuthorUsername: "artist", Media: &postMedia{
				Photos: slices.Repeat([]postImage{{URL: "photo"}}, 5),
			}}
			request := renderRequest{Theme: "light", Post: post, Images: map[string][]byte{"photo": asset.Bytes()}}
			if tc.compact {
				request.Post = renderPost{AuthorName: "Artist", AuthorUsername: "artist"}
				request.ReplyChain = []renderPost{post}
			}
			data, err := renderPostImage(request)
			if err != nil {
				t.Fatal(err)
			}
			picture, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			x := 1036
			if tc.compact {
				x -= 48
			}
			if arrow := carouselArrowVisible(picture, x); arrow != tc.arrow {
				t.Fatalf("arrow = %t, want %t", arrow, tc.arrow)
			}
		})
	}
}

func carouselArrowVisible(picture image.Image, x int) bool {
	dark := 0
	for y := 250; y < 700; y++ {
		for dx := -20; dx <= 20; dx++ {
			r, g, b, _ := picture.At(x+dx, y).RGBA()
			if r < 40000 && g < 40000 && b < 40000 {
				dark++
			}
		}
	}
	return dark > 100
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
	request, err := preparePostImage(link, "light", defaultTimezone)
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

			request, err := preparePostImage(inlineLink("https://x.com/author/status/7"), "light", defaultTimezone)
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

type postPanelFixture struct {
	name      string
	widths    []int
	height    int
	carousel  bool
	unrelated bool
}

func postPanelServer(t *testing.T, assets [][]byte) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if index, ok := strings.CutPrefix(r.URL.Path, "/panel/"); ok {
			i, err := strconv.Atoi(index)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = w.Write(assets[i])
			return
		}
		if r.URL.Path == "/bad-mosaic" {
			t.Error("fetched remote mosaic")
			http.Error(w, "bad mosaic", 500)
			return
		}
		photos := make([]postImage, len(assets))
		for i := range photos {
			photos[i].URL = fmt.Sprintf("%s/panel/%d", server.URL, i)
		}
		mosaic := map[string]any{"formats": map[string]string{"jpeg": server.URL + "/bad-mosaic"}}
		media := map[string]any{"photos": photos, "mosaic": mosaic}
		author := map[string]string{"name": "Artist", "screen_name": "artist"}
		post := map[string]any{"author": author, "media": media}
		if r.URL.Path == "/artist/status/42" {
			post["quote"] = map[string]any{"author": author, "media": media}
			post["replying_to_status"] = "41"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": post})
	}))
	return server
}

func checkPostPanels(t *testing.T, req renderRequest, tc postPanelFixture, server *httptest.Server) {
	t.Helper()
	for _, post := range []*renderPost{&req.Post, req.Post.Quote, &req.ReplyChain[0]} {
		m := post.Media
		if m.Carousel != tc.carousel {
			t.Fatalf("carousel: %+v", m)
		}
		if tc.carousel {
			if len(m.Photos) != 20 {
				t.Fatalf("photos %d", len(m.Photos))
			}
			for i, photo := range m.Photos {
				if photo.URL != fmt.Sprintf("%s/panel/%d", server.URL, i) {
					t.Fatal("panel order", i, photo.URL)
				}
			}
			continue
		}
		want := 1
		if tc.unrelated {
			want = 2
		}
		if len(m.Photos) != want {
			t.Fatalf("photos %d", len(m.Photos))
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(req.Images[m.Photos[0].URL]))
		if err != nil || cfg.Width != 552 || cfg.Height != 552 {
			t.Fatalf("merged size %+v: %v", cfg, err)
		}
		if tc.unrelated && m.Photos[1].URL != server.URL+"/panel/20" {
			t.Fatal("unrelated photo reordered")
		}
	}
}

func TestPostImageOriginalPanels(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	widths := []int{28, 27, 28, 27, 28, 28, 27, 28, 27, 28, 28, 27, 28, 27, 28, 28, 27, 28, 27, 28}
	for _, tc := range []postPanelFixture{
		{name: "streetysnail", widths: widths, height: 552},
		{name: "streetysnail", widths: widths, height: 552, unrelated: true},
		{name: "reyforn-panorama", widths: slices.Repeat([]int{1080}, 20), height: 1440, carousel: true},
	} {
		t.Run(fmt.Sprintf("%s-unrelated-%t", tc.name, tc.unrelated), func(t *testing.T) {
			fixtures := artFixturePhotos(t, tc.name, tc.widths, tc.height)
			assets := make([][]byte, len(fixtures))
			for i, item := range fixtures {
				assets[i] = item.(tgbotapi.InputMediaPhoto).Media.(tgbotapi.FileBytes).Bytes
			}
			if tc.unrelated {
				var data bytes.Buffer
				if err := png.Encode(&data, artScene(image.Rect(0, 0, 200, 552), 2)); err != nil {
					t.Fatal(err)
				}
				assets = append(assets, data.Bytes())
			}
			server := postPanelServer(t, assets)
			defer server.Close()
			target, _ := url.Parse(server.URL)
			client := &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
			req, err := preparePostImageWithClient(client, inlineLink("https://x.com/artist/status/42"), "light", defaultTimezone)
			if err != nil {
				t.Fatal(err)
			}
			checkPostPanels(t, req, tc, server)
			for _, theme := range []string{"light", "dark"} {
				req.Theme = theme
				if _, err := renderPostImage(req); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPostImageTimezonePreparation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		post := map[string]any{
			"created_timestamp": 1704065400,
			"author":            map[string]string{"name": "Post", "screen_name": "post"},
			"quote": map[string]any{
				"created_timestamp": 1719835200,
				"author":            map[string]string{"name": "Quoted", "screen_name": "quote"},
			},
		}

		if !strings.Contains(r.URL.Path, "/status/41") {
			post["replying_to_status"] = "41"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": post})
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	for _, tc := range []struct{ zone, winter, summer string }{
		{defaultTimezone, "2:30 AM · Jan 1, 2024 UTC+03:00", "3:00 PM · Jul 1, 2024 UTC+03:00"},
		{"America/New_York", "6:30 PM · Dec 31, 2023 UTC-05:00", "8:00 AM · Jul 1, 2024 UTC-04:00"},
		{"Asia/Kathmandu", "5:15 AM · Jan 1, 2024 UTC+05:45", "5:45 PM · Jul 1, 2024 UTC+05:45"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			request, err := preparePostImageWithClient(client, inlineLink("https://x.com/post/status/42"), "light", tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			if len(request.ReplyChain) != 1 {
				t.Fatalf("reply chain: %+v", request.ReplyChain)
			}
			for _, post := range []renderPost{request.Post, request.ReplyChain[0]} {
				if post.CreatedAt != tc.winter || post.Quote == nil || post.Quote.CreatedAt != tc.summer {
					t.Fatalf("timestamps: %+v quote=%+v", post, post.Quote)
				}
			}
			data, err := json.Marshal(request)
			if err != nil || bytes.Contains(data, []byte("created_timestamp")) {
				t.Fatalf("JSON %s: %v", data, err)
			}
		})
	}
}

func TestPostImageCacheSharedByTimezone(t *testing.T) {
	db := testArtworkDB(t)
	if err := saveUserTimezone(db, 3, "UTC"); err != nil {
		t.Fatal(err)
	}

	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 64, 64)), nil); err != nil {
		t.Fatal(err)
	}
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
	renderer := testPostImageRenderer(t)
	t.Setenv("KOPOKA_RENDERER", renderer)

	link := inlineLink("https://x.com/user/status/42")
	choose := func(user int64) {
		t.Helper()
		chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: user}, InlineMessageID: "inline"}
		if err := editInlinePostImage(bot, db, chosen, "@cache", link, "light", nil); err != nil {
			t.Fatal(err)
		}
	}
	choose(1)
	before := len(calls)
	t.Setenv("KOPOKA_RENDERER", filepath.Join(t.TempDir(), "missing"))
	choose(2)
	if strings.Join(calls[before:], ",") != "edit" {
		t.Fatalf("same zone calls: %v", calls[before:])
	}
	t.Setenv("KOPOKA_RENDERER", renderer)
	before = len(calls)
	choose(3)
	if !slices.Contains(calls[before:], "fetch") || !slices.Contains(calls[before:], "upload") {
		t.Fatalf("different zone calls: %v", calls[before:])
	}
	var count int
	id := artworkID(xPostLink(link))
	row := db.QueryRow(`SELECT count(*) FROM post_image_cache WHERE artwork_id=? AND theme='light'`, id)
	if err := row.Scan(&count); err != nil || count != 2 {
		t.Fatalf("cache rows %d: %v", count, err)
	}
	if _, err := db.Exec(`UPDATE post_image_cache SET render_version=? WHERE timezone=?`, postImageVersion-1, "UTC"); err != nil {
		t.Fatal(err)
	}
	file, _, err := findPostImage(db, id, "light", "UTC")
	if err != nil || file != "" {
		t.Fatalf("stale cache file %q: %v", file, err)
	}
}
