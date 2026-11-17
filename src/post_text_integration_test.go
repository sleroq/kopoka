package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

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
				"text": "Main <text> & 50% \\%",
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
		id := r.URL.Path[len(r.URL.Path)-1:]
		parent := map[string]string{"4": "3", "3": "2", "2": "1"}[id]
		s.paths = append(s.paths, r.URL.Path)
		_, _ = fmt.Fprintf(w, `{
			"code": 200,
			"tweet": {
				"text": "Parent %s",
				"author": {"name": "Author %s", "screen_name": "parent%s"},
				"replying_to_status": "%s",
				"media": {"photos": [{"url": "https://media.example/parent.jpg"}]}
			}
		}`, id, id, id, parent)
	case "/user/status/6":
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"Text only","author":{"name":"Solo","screen_name":"user"}}}`)
	case "/user/status/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Long", "screen_name": "user"},
			"text":   strings.Repeat("😀", 2100),
			"media": map[string]any{
				"photos": []map[string]string{{"url": "https://media.example/long.jpg"}},
			},
		}})
	case "/user/status/8":
		_, _ = fmt.Fprint(w, `{
			"code": 200,
			"tweet": {
				"text": "Still here",
				"author": {"name": "Reply", "screen_name": "user"},
				"replying_to_status": "99"
			}
		}`)
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
	case "/i/status/99":
		http.Error(w, "unavailable", http.StatusNotFound)
	default:
		s.telegram(w, r)
	}
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
	for _, rich := range []bool{false, true} {
		for _, failure := range []string{"Bad Request: failed to get HTTP URL content", "Bad Request: unrelated"} {
			t.Run(fmt.Sprintf("rich=%t/%s", rich, failure), func(t *testing.T) {
				db := testArtworkDB(t)
				if rich {
					if err := toggleXPostFormat(db, 1); err != nil {
						t.Fatal(err)
					}
				}

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
				handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
					From: &tgbotapi.User{ID: 1}, ResultID: inlinePostTextID, InlineMessageID: "inline",
					Query: "https://x.com/user/status/12",
				}, "@cache", queue, nil)
				want := []string{"https://media.example/original.mp4"}
				if strings.Contains(failure, "failed to get HTTP URL content") {
					want = append(want, "https://media.example/mid.mp4", "https://media.example/low.mp4")
					if len(api.edits) != 1 || (!strings.Contains(api.edits[0].Get("media"), "file3") &&
						!strings.Contains(api.edits[0].Get("rich_message"), "file3")) {
						t.Fatalf("expected cached video in edit: %#v", api.edits)
					}
				}

				if !slices.Equal(api.uploads, want) {
					t.Fatalf("uploads = %v, want %v", api.uploads, want)
				}
			})
		}
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
	handleInlineQuery(bot, db, query, previews)
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
	choose := func(id int64, link string) url.Values {
		t.Helper()
		before := len(api.edits)
		handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: id}, ResultID: inlinePostTextID, InlineMessageID: "inline", Query: link,
		}, "@cache", queue, nil)
		if len(api.edits) != before+1 {
			t.Fatalf("expected edit: %#v", api.edits)
		}
		return api.edits[before]
	}

	testXPostOrdinaryFormat(t, api, choose, link)
	if err := toggleXPostFormat(db, 1); err != nil {
		t.Fatal(err)
	}
	api.uploads, api.paths = nil, nil
	testXPostRichFormat(t, api, choose, link)
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
	for _, text := range []string{`Reply thread from <a href="https://x.com/user/status/5">X</a>`, "Author 2", "Parent 3", "Parent 4", "Main &amp; Co", `Main &lt;text&gt; &amp; 50% \%`, `href="https://x.com/quoted/status/9"`, "Quoted text", "<tg-slideshow>"} {
		if !strings.Contains(richPayload.HTML, text) {
			t.Errorf("rich message missing %q: %s", text, richPayload.HTML)
		}
	}
	if strings.Count(richPayload.HTML, "<blockquote expandable>") != 3 ||
		strings.Index(richPayload.HTML, "Parent 2") > strings.Index(richPayload.HTML, "Parent 3") ||
		strings.Index(richPayload.HTML, "Parent 3") > strings.Index(richPayload.HTML, "Parent 4") ||
		strings.Index(richPayload.HTML, "tg://video?id=1") > strings.Index(richPayload.HTML, "Quoted text") ||
		strings.Index(richPayload.HTML, "Quoted text") > strings.Index(richPayload.HTML, "tg://photo?id=2") {
		t.Fatalf("rich thread or media order: %s", richPayload.HTML)
	}
	if len(richPayload.Media) != 3 || richPayload.Media[0].Media.Type != "photo" || richPayload.Media[1].Media.Type != "video" || richPayload.Media[2].Media.Media != "file3" || strings.Join(api.uploads, ",") != "https://media.example/main.jpg,https://media.example/main.mp4,https://media.example/quote.jpg" {
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

}

func testXPostOrdinaryFormat(t *testing.T, api *postTextServer, choose func(int64, string) url.Values, link string) {
	t.Helper()
	normal := choose(1, link)
	var media map[string]any
	if err := json.Unmarshal([]byte(normal.Get("media")), &media); err != nil {
		t.Fatal(err)
	}
	caption := media["caption"].(string)
	if media["type"] != "video" || media["media"] != "file1" || media["show_caption_above_media"] != true ||
		strings.Count(caption, "<blockquote expandable>") != 3 || !strings.Contains(caption, `<blockquote><a href="https://x.com/quoted/status/9">Quoted</a>`) ||
		strings.Contains(caption, "Attachment ") || strings.Contains(caption, "media.example") {
		t.Fatalf("normal caption: %#v", media)
	}
	if len(api.uploads) != 1 || api.uploads[0] != "video:multipart" {
		t.Fatalf("normal uploads: %v", api.uploads)
	}

	frame := motionTestFrame(t, api.composed, "0.1")
	height := frame.Bounds().Dy()
	motionAssertColor(t, frame, height/2, height/2, 2)
	motionAssertColor(t, frame, frame.Bounds().Dx()/2, height/2, 0)
	motionAssertColor(t, frame, frame.Bounds().Dx()-height/2, height/2, 0)

	textOnly := choose(1, "https://x.com/user/status/6")
	if textOnly.Get("parse_mode") != "HTML" || !strings.Contains(textOnly.Get("text"), "Text only") || textOnly.Get("media") != "" {
		t.Fatalf("text-only post: %v", textOnly)
	}
	testXPostOrdinaryLimits(t, choose)
	quoted := choose(1, "https://x.com/user/status/11")
	if err := json.Unmarshal([]byte(quoted.Get("media")), &media); err != nil {
		t.Fatal(err)
	}
	if media["type"] != "video" || media["media"] != "file3" ||
		!strings.Contains(media["caption"].(string), `href="https://x.com/quoted/status/12"`) ||
		len(api.uploads) != 3 || api.uploads[1] != "https://media.example/long.jpg" || api.uploads[2] != "https://media.example/quoted.mp4" {
		t.Fatalf("quoted attachment: %#v, uploads %v", media, api.uploads)
	}
	partial := choose(1, "https://x.com/user/status/8")
	if !strings.Contains(partial.Get("text"), "Still here") || !strings.Contains(partial.Get("text"), "…") {
		t.Fatalf("inaccessible parent: %v", partial)
	}
}

func testXPostOrdinaryLimits(t *testing.T, choose func(int64, string) url.Values) {
	t.Helper()
	long := choose(1, "https://x.com/user/status/7")
	var media map[string]any
	if err := json.Unmarshal([]byte(long.Get("media")), &media); err != nil {
		t.Fatal(err)
	}
	caption := media["caption"].(string)
	visible := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(caption, ""))
	if media["type"] != "photo" || media["media"] != "file2" || media["show_caption_above_media"] != true ||
		len(utf16.Encode([]rune(visible))) > 1024 || !strings.Contains(visible, "…") || strings.Contains(caption, "Attachment ") {
		t.Fatalf("long caption: %d units, %#v", len(utf16.Encode([]rune(visible))), media)
	}
	longText := choose(1, "https://x.com/user/status/10")
	visible = html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(longText.Get("text"), ""))
	if len(utf16.Encode([]rune(visible))) > 4096 || !strings.Contains(visible, "…") || longText.Get("media") != "" {
		t.Fatalf("long text: %d units, %s", len(utf16.Encode([]rune(visible))), longText.Get("text"))
	}
}
