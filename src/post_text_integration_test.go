package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type postTextMediaTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r postTextMediaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "media.example" {
		copy := req.Clone(req.Context())
		u := *req.URL
		u.Scheme, u.Host = r.target.Scheme, r.target.Host
		copy.URL = &u
		return r.base.RoundTrip(copy)
	}
	return redirectTransport(r).RoundTrip(req)
}

type postTextServer struct {
	t            *testing.T
	mu           sync.Mutex
	results      []map[string]any
	edits        []url.Values
	uploads      []string
	uploadErrors map[string]string
	paths        []string
	message      url.Values
	fixtures     map[string][]byte
	composed     []byte
}

func (s *postTextServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveLimitFixture(w, r.URL.Path) {
		return
	}
	switch r.URL.Path {
	case "/main.jpg", "/main.mp4", "/quote.jpg":
		data := s.fixtures[r.URL.Path]
		if data == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	case "/user/status/5":
		_, _ = fmt.Fprint(w, `{
			"code": 200,
			"tweet": {
				"id": "5", "url": "https://x.com/user/status/5",
				"text": "Main <text> & 50% \\% @alice https://example.com/path?a=1&b=2. email@example.com",
				"author": {"name": "Main & Co", "screen_name": "user"},
				"replying_to_status": "4",
				"media": {"all": [
					{"type": "photo", "url": "https://media.example/main.jpg"},
					{"type": "video", "url": "https://media.example/main.mp4"}
				]},
				"quote": {
					"id": "9", "url": "https://x.com/quoted/status/9", "text": "Quoted text",
					"author": {"name": "Quoted", "screen_name": "quoted"},
					"media": {"photos": [{"url": "https://media.example/quote.jpg"}]}
				}
			}
		}`)
	case "/i/status/4", "/i/status/3", "/i/status/2":
		s.serveReplyFixture(w, r.URL.Path)
	case "/user/status/6":
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"Text only","author":{"name":"User","screen_name":"user"}}}`)
	case "/user/status/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Long", "screen_name": "user"},
			"text":   strings.Repeat("😀", 2100),
			"media": map[string]any{
				"photos": []map[string]string{{"url": "https://media.example/long.jpg"}},
			},
		}})
	case "/user/status/9":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Long", "screen_name": "user"}, "text": strings.Repeat("a", 20000),
			"quote": map[string]any{"author": map[string]string{"name": "Quoted", "screen_name": "q"}, "text": strings.Repeat("b", 20000)},
		}})
	case "/user/status/10":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Long", "screen_name": "user"}, "text": strings.Repeat("😀", 2100),
		}})
	case "/user/status/11":
		_, _ = fmt.Fprint(w, `{
			"code": 200,
			"tweet": {
				"author": {"name": "Author", "screen_name": "user"},
				"quote": {
					"id": "12", "author": {"name": "Quoted", "screen_name": "quoted"},
					"media": {"videos": [{"url": "https://media.example/quoted.mp4"}]}
				}
			}
		}`)
	case "/user/status/12":
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"Video","author":{"name":"Author","screen_name":"user"},
			"media":{"all":[{"type":"video","url":"https://media.example/original.mp4","formats":[
				{"url":"https://media.example/low.mp4","container":"mp4","bitrate":256000},
				{"url":"https://media.example/original.mp4","container":"mp4","bitrate":3000000},
				{"url":"https://media.example/stream.m3u8","container":"m3u8","bitrate":2000000},
				{"url":"https://media.example/mid.mp4","container":"mp4","bitrate":832000},
				{"url":"https://media.example/mid.mp4","container":"mp4","bitrate":832000}
			]}]}}}`)

	case "/user/status/8", "/user/status/15", "/user/status/16":
		extra := ""
		switch r.URL.Path {
		case "/user/status/8":
			extra = `, "replying_to_status": "99"`
		case "/user/status/16":
			extra = `, "quote": {"text": "Quoted post", "author": {"name": "Quoted"}}`
		}
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{
			"text":"Photo <text> & @alice\nSecond line",
			"author":{"name":"Artist & Co","screen_name":"user"},
			"media":{"photos":[{"url":"https://media.example/main.jpg"}]}%s
		}}`, extra)
	case "/i/status/99":
		http.Error(w, "unavailable", http.StatusNotFound)
	default:
		s.telegram(w, r)
	}
}

func (s *postTextServer) serveReplyFixture(w http.ResponseWriter, path string) {
	id := path[len(path)-1:]
	quote := ""
	if id == "4" {
		quote = `, "quote": {
			"id": "44", "author": {"name": "Parent quote", "screen_name": "pq"},
			"text": "Hidden parent quote",
			"media": {"photos": [{"url": "https://media.example/parent-quote.jpg"}]}
		}`
	}
	parent := map[string]string{"4": "3", "3": "2", "2": "1"}[id]
	s.paths = append(s.paths, path)
	_, _ = fmt.Fprintf(w, `{
		"code": 200,
		"tweet": {
			"text": "Parent %s",
			"author": {"name": "Author %s", "screen_name": "parent%s"},
			"replying_to_status": "%s",
			"media": {"photos": [{"url": "https://media.example/parent.jpg"}]}%s
		}
	}`, id, id, id, parent, quote)
}

func (s *postTextServer) serveLimitFixture(w http.ResponseWriter, path string) bool {
	switch path {
	case "/user/status/13", "/user/status/14":
		id := strings.TrimPrefix(path, "/user/status/")
		_, _ = fmt.Fprintf(w, `{
			"code": 200,
			"tweet": {"text": "Focus", "author": {"name": "Focus", "screen_name": "user"},
				"replying_to_status": "%s"}
		}`, id)
	case "/i/status/13":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Parent", "screen_name": "parent"},
			"quote": map[string]any{
				"author": map[string]string{"name": "Parent quote", "screen_name": "pq"},
				"text":   strings.Repeat("z", 40000),
			},
		}})
	case "/i/status/14":
		s.paths = append(s.paths, path)
		photos := make([]map[string]string, 51)
		for i := range photos {
			photos[i] = map[string]string{"url": "https://media.example/parent-quote.jpg"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Parent", "screen_name": "parent"},
			"quote": map[string]any{
				"author": map[string]string{"name": "Quote", "screen_name": "q"},
				"media":  map[string]any{"photos": photos},
			},
		}})
	default:
		return false
	}
	return true
}

func (s *postTextServer) telegram(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		if err := json.Unmarshal([]byte(r.FormValue("results")), &s.results); err != nil {
			s.t.Error(err)
		}

		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"), strings.HasSuffix(r.URL.Path, "/sendVideo"):
		kind, field := "photo", "photo"
		if strings.HasSuffix(r.URL.Path, "/sendVideo") {
			kind, field = "video", "video"
		}
		upload := r.FormValue(field)
		if file, _, err := r.FormFile(field); err == nil {
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				s.t.Error(readErr)
			}
			s.composed = data
			upload = kind + ":multipart"
		}
		s.uploads = append(s.uploads, upload)
		if message := s.uploadErrors[r.FormValue(field)]; message != "" {
			_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":%q}`, message)
			return
		}
		id := fmt.Sprintf("file%d", len(s.uploads))
		if kind == "photo" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":42,"photo":[{"file_id":%q}]}}`, id)
		} else {
			_, _ = fmt.Fprintf(w, `{
				"ok": true,
				"result": {
					"message_id": 42,
					"video": {"file_id": %q, "width": 1, "height": 1, "duration": 1}
				}
			}`, id)
		}
	case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
		s.edits = append(s.edits, r.Form)
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		s.message = r.Form
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":44}}`)
	default:
		s.t.Errorf("unexpected request: %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestXPostInlineVideoURLRetry(t *testing.T) {
	for _, failure := range []string{"Bad Request: failed to get HTTP URL content", "Bad Request: unrelated"} {
		t.Run(failure, func(t *testing.T) {
			db := testArtworkDB(t)

			api := &postTextServer{t: t, uploadErrors: map[string]string{
				"https://media.example/original.mp4": failure,
				"https://media.example/mid.mp4":      "Bad Request: failed to get HTTP URL content",
			}}
			server := httptest.NewServer(api)
			defer server.Close()
			target, _ := url.Parse(server.URL)
			original := http.DefaultTransport
			http.DefaultTransport = redirectTransport{target, original}
			defer func() { http.DefaultTransport = original }()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
			handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlinePostTextID, InlineMessageID: "inline",
				Query: "https://x.com/user/status/12",
			}, "@cache", queue, nil)
			want := []string{"https://media.example/original.mp4"}
			if strings.Contains(failure, "failed to get HTTP URL content") {
				want = append(want, "https://media.example/mid.mp4", "https://media.example/low.mp4")
				if len(api.edits) != 1 || !strings.Contains(api.edits[0].Get("rich_message"), "file3") {
					t.Fatalf("expected cached video in edit: %#v", api.edits)
				}
			}

			if !slices.Equal(api.uploads, want) {
				t.Fatalf("uploads = %v, want %v", api.uploads, want)
			}
		})
	}
}

func TestXPostRichMessageActions(t *testing.T) {
	db := testArtworkDB(t)
	api := &postTextServer{t: t}
	server := httptest.NewServer(api)
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	var message telegramMessage
	data := `{"message_id":42,"from":{"id":1},"chat":{"id":123,"type":"private"},"via_bot":{"id":1,"is_bot":true,"first_name":"bot"},
		"rich_message":{"blocks":[{"type":"paragraph","text":["Reply thread from ",{"type":"url","text":"X","url":"https://x.com/user/status/5"},":"]},
		{"type":"expandable_blockquote","text":{"type":"url","text":"Parent","url":"https://x.com/parent/status/4"}}]}}`
	if err := json.Unmarshal([]byte(data), &message); err != nil {
		t.Fatal(err)
	}
	handleIncomingMessage(bot, nil, db, &message, nil, nil)
	var keyboard tgbotapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(api.message.Get("reply_markup")), &keyboard); err != nil {
		t.Fatal(err)
	}
	if len(keyboard.InlineKeyboard) != 3 || keyboard.InlineKeyboard[2][0].SwitchInlineQueryCurrentChat == nil ||
		*keyboard.InlineKeyboard[2][0].SwitchInlineQueryCurrentChat != "https://x.com/user/status/5" {
		t.Fatalf("wrong post action: %s", api.message.Get("reply_markup"))
	}
}

func TestXPostInlinePhotoCaption(t *testing.T) {
	db := testArtworkDB(t)
	api := &postTextServer{t: t}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	t.Cleanup(func() { http.DefaultTransport = original })
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})

	for _, tc := range []struct{ name, id, richText string }{
		{name: "single photo", id: "15"},
		{name: "oversize caption", id: "7", richText: strings.Repeat("😀", 2100)},
		{name: "reply with missing parent", id: "8", richText: "Reply thread on"},
		{name: "quote", id: "16", richText: "Quoted post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(api.edits)
			handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlinePostTextID, InlineMessageID: "inline",
				Query: "https://x.com/user/status/" + tc.id,
			}, "@cache", queue, nil)
			if len(api.edits) != before+1 {
				t.Fatalf("expected one edit, got %d", len(api.edits)-before)
			}
			edit := api.edits[before]
			if tc.richText != "" {
				if edit.Get("media") != "" || !strings.Contains(edit.Get("rich_message"), tc.richText) {
					t.Fatalf("expected rich message, got %v", edit)
				}
				return
			}
			type photoEdit struct {
				Type              string `json:"type"`
				Media             string `json:"media"`
				Caption           string `json:"caption"`
				ParseMode         string `json:"parse_mode"`
				CaptionAboveMedia bool   `json:"show_caption_above_media"`
			}
			var photo photoEdit
			if err := json.Unmarshal([]byte(edit.Get("media")), &photo); err != nil {
				t.Fatalf("expected photo edit: %v (%v)", err, edit)
			}
			want := photoEdit{
				Type: "photo", Media: "file1", ParseMode: "HTML", CaptionAboveMedia: true,
				Caption: `<b><a href="https://x.com/user/status/15">Artist &amp; Co</a></b> on X` + "\n\n" +
					`Photo &lt;text&gt; &amp; <a href="https://x.com/alice">@alice</a>` + "\nSecond line",
			}
			if photo != want || edit.Get("rich_message") != "" {
				t.Fatalf("unexpected photo edit: %v", edit)
			}
		})
	}
}

func TestXPostInlineFormats(t *testing.T) {
	motionTestTools(t)
	db := testArtworkDB(t)
	if _, err := db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}
	api := &postTextServer{t: t, fixtures: map[string][]byte{
		"/main.jpg":  motionTestPNG(t, color.RGBA{B: 255, A: 255}),
		"/main.mp4":  postMergeTestVideo(t),
		"/quote.jpg": motionTestPNG(t, color.RGBA{R: 255, A: 255}),
	}}
	server := httptest.NewServer(api)
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = postTextMediaTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	link := "https://x.com/user/status/5"
	query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: link}
	previews := testPreviews(t, db, bot)
	handleInlineQuery(time.Now(), bot, db, query, previews)
	for _, result := range api.results {
		if result["type"] != "photo" {
			t.Fatalf("non-photo result breaks gallery: %#v", result)
		}
	}
	result := api.results[len(api.results)-1]
	icon := "https://preview.example.com/post-text-placeholder.jpg"
	content := result["input_message_content"].(map[string]any)
	if result["id"] != inlinePostTextID || result["photo_url"] != icon || result["thumbnail_url"] != icon ||
		result["photo_width"] != float64(384) || result["photo_height"] != float64(384) || content["message_text"] != "Loading X post…" {
		t.Fatalf("post result: %#v", result)
	}
	response := httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", icon, nil))
	image, err := jpeg.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
	if err != nil || response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" ||
		image.Width != 384 || image.Height != 384 || bytes.Equal(response.Body.Bytes(), previews.placeholders[false]) {
		t.Fatalf("post icon: status %d, image %+v, error %v", response.Code, image, err)
	}
	previews.client = &http.Client{}
	previewLink, _ := url.Parse(link)
	blocks, err := previews.postTextPreviewBlocks(context.Background(), previewLink)
	if err != nil {
		t.Fatal(err)
	}
	var previewText []string
	for _, block := range blocks {
		previewText = append(previewText, block.Text)
	}
	visible := strings.Join(previewText, "|")
	if !strings.Contains(visible, "Author 4") || !strings.Contains(visible, "Quoted") ||
		!strings.Contains(visible, "Main <text>") || strings.Contains(visible, "Parent 4") ||
		strings.Contains(visible, "Quoted text") || strings.Contains(visible, "Parent quote") {
		t.Fatalf("collapsed preview: %#v", blocks)
	}
	api.paths = nil
	choose := func(id int64, link string) url.Values {
		t.Helper()
		before := len(api.edits)
		handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: id}, ResultID: inlinePostTextID, InlineMessageID: "inline", Query: link,
		}, "@cache", queue, nil)
		if len(api.edits) != before+1 {
			t.Fatalf("expected edit: %#v", api.edits)
		}
		return api.edits[before]
	}

	testXPostRichFormat(t, api, choose, link)
	capLink, _ := url.Parse("https://x.com/user/status/14")
	before := len(api.uploads)
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, InlineMessageID: "inline",
	}
	err = editInlinePostText(context.Background(), bot, db, chosen, "@cache", capLink, nil)
	if err == nil || api.paths[len(api.paths)-1] != "/i/status/14" || len(api.uploads) != before {
		t.Fatalf("attachment cap: error %v, uploads %v, paths %v", err, api.uploads[before:], api.paths)
	}

	for _, user := range []int64{2, 3} {
		edit := choose(user, "https://x.com/user/status/6")
		if !strings.Contains(edit.Get("rich_message"), "Text only") || strings.Contains(edit.Get("rich_message"), "@user") {
			t.Fatalf("user %d received unexpected rich text: %v", user, edit)
		}
	}
}

func testXPostRichFormat(t *testing.T, api *postTextServer, choose func(int64, string) url.Values, link string) {
	t.Helper()
	rich := choose(1, link)
	var richPayload struct {
		HTML  string `json:"html"`
		Media []struct {
			ID    string `json:"id"`
			Media struct {
				Type  string `json:"type"`
				Media string `json:"media"`
			} `json:"media"`
		} `json:"media"`
	}
	if err := json.Unmarshal([]byte(rich.Get("rich_message")), &richPayload); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`Reply thread on <a href="https://x.com/user/status/5">X</a>:`, "Author 2", "Parent 3", "Parent 4", "Main &amp; Co", `Main &lt;text&gt; &amp; 50% \% <a href="https://x.com/alice">@alice</a> <a href="https://example.com/path?a=1&amp;b=2">https://example.com/path?a=1&amp;b=2</a>. email@example.com`, `href="https://x.com/quoted/status/9"`, "Quoted text", "<tg-slideshow>"} {
		if !strings.Contains(richPayload.HTML, text) {
			t.Errorf("rich message missing %q: %s", text, richPayload.HTML)
		}
	}
	if strings.Count(richPayload.HTML, "<details><summary>") != 5 ||
		!strings.Contains(richPayload.HTML, `<details><summary><b><a href="https://x.com/i/status/4">Author 4</a></b></summary><p>Parent 4</p><img src="tg://photo?id=5"/><details><summary><b><a href="https://x.com/pq/status/44">Parent quote</a></b></summary><p>Hidden parent quote</p><img src="tg://photo?id=6"/></details></details>`) ||
		!strings.Contains(richPayload.HTML, `</details><p><br><b><a href="https://x.com/user/status/5">Main &amp; Co</a></b>:</p>`) ||
		!strings.Contains(richPayload.HTML, `</tg-slideshow><details><summary>`) ||
		strings.Index(richPayload.HTML, "Parent 2") > strings.Index(richPayload.HTML, "Parent 3") ||
		strings.Index(richPayload.HTML, "Parent 3") > strings.Index(richPayload.HTML, "Parent 4") ||
		strings.Index(richPayload.HTML, "tg://video?id=1") > strings.Index(richPayload.HTML, "Quoted text") ||
		strings.Index(richPayload.HTML, "Quoted text") > strings.Index(richPayload.HTML, "tg://photo?id=2") {
		t.Fatalf("rich thread or media order: %s", richPayload.HTML)
	}
	if len(richPayload.Media) != 7 || richPayload.Media[0].Media.Type != "photo" || richPayload.Media[1].Media.Type != "video" || richPayload.Media[6].Media.Media != "file7" || strings.Join(api.uploads, ",") != "https://media.example/main.jpg,https://media.example/main.mp4,https://media.example/quote.jpg,https://media.example/parent.jpg,https://media.example/parent.jpg,https://media.example/parent.jpg,https://media.example/parent-quote.jpg" {
		t.Fatalf("rich attachments: %#v, uploads %v", richPayload.Media, api.uploads)
	}

	if strings.Join(api.paths, ",") != "/i/status/4,/i/status/3,/i/status/2" {
		t.Fatalf("resolved parents: %v", api.paths)
	}
	longRich := choose(1, "https://x.com/user/status/9")
	var longPayload struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal([]byte(longRich.Get("rich_message")), &longPayload); err != nil {
		t.Fatal(err)
	}
	richVisible := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(longPayload.HTML, ""))
	if len([]rune(richVisible)) > 32768 || !strings.Contains(richVisible, "…") || !strings.Contains(richVisible, "Quoted") {
		t.Fatalf("long rich post: %d characters", len([]rune(richVisible)))
	}

	parentRich := choose(1, "https://x.com/user/status/13")
	if err := json.Unmarshal([]byte(parentRich.Get("rich_message")), &longPayload); err != nil {
		t.Fatal(err)
	}
	richVisible = html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(longPayload.HTML, ""))
	if len([]rune(richVisible)) > 32768 || !strings.Contains(richVisible, "…") || !strings.Contains(richVisible, "Parent quote") {
		t.Fatalf("long parent quote: %d characters", len([]rune(richVisible)))
	}

}

func TestLocalPostTextUploadsOriginalVideo(t *testing.T) {
	for _, uploadFail := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload failure=%t", uploadFail), func(t *testing.T) {
			t.Setenv("TELEGRAM_BOT_API_URL", "http://local")
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/original.mp4" {
					t.Errorf("downloaded fallback: %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, "original video")
			}))
			defer source.Close()
			var sends []string
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/deleteMessage") {
					_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
					return
				}
				if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
					file, _, err := r.FormFile("video")
					if err != nil {
						t.Error(err)
						return
					}
					data, err := io.ReadAll(file)
					_ = file.Close()
					if err != nil || string(data) != "original video" {
						t.Errorf("upload: %q %v", data, err)
					}
					sends = append(sends, "upload")
					if uploadFail {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"upload rejected"}`)
						return
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"video":{"file_id":"original-id"}}}`)
					return
				}
				sends = append(sends, r.FormValue("video"))
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"failed to get HTTP URL content"}`)
			}))
			defer telegram.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			item := xMedia{Type: "video", URL: source.URL + "/original.mp4", FallbackURLs: []string{source.URL + "/low.mp4"}}
			id, err := cachePostMediaItem(bot, "42", item, inputPostMedia(item), nil)
			if (err != nil) != uploadFail || (!uploadFail && id != "original-id") || !slices.Equal(sends, []string{item.URL, "upload"}) {
				t.Fatalf("id=%s error=%v sends=%v", id, err, sends)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Errorf("temporary files: %v %v", files, err)
			}
		})
	}
}
