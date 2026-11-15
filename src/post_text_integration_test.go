package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type postTextServer struct {
	t       *testing.T
	mu      sync.Mutex
	results []map[string]any
	edits   []url.Values
	uploads []string
	paths   []string
	message url.Values
}

func (s *postTextServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.URL.Path {
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
		s.uploads = append(s.uploads, r.FormValue(field))
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
	db := testArtworkDB(t)
	if _, err := db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}
	api := &postTextServer{t: t}
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
	link := "https://x.com/user/status/5"
	query := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: link}
	handleInlineQuery(bot, db, query, testPreviews(t, db, bot))
	result := api.results[len(api.results)-1]
	if result["id"] != inlinePostTextID || result["type"] != "article" {
		t.Fatalf("post result: %#v", result)
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
	if media["type"] != "photo" || media["media"] != "file1" || media["show_caption_above_media"] != true ||
		strings.Count(caption, "<blockquote expandable>") != 3 || !strings.Contains(caption, `<blockquote><a href="https://x.com/quoted/status/9">Quoted</a>`) ||
		!strings.Contains(caption, `href="https://media.example/main.mp4"`) || !strings.Contains(caption, `href="https://media.example/quote.jpg"`) {
		t.Fatalf("normal caption: %#v", media)
	}
	if len(api.uploads) != 1 || api.uploads[0] != "https://media.example/main.jpg" {
		t.Fatalf("normal uploads: %v", api.uploads)
	}

	textOnly := choose(1, "https://x.com/user/status/6")
	if textOnly.Get("parse_mode") != "HTML" || !strings.Contains(textOnly.Get("text"), "Text only") || textOnly.Get("media") != "" {
		t.Fatalf("text-only post: %v", textOnly)
	}
	long := choose(1, "https://x.com/user/status/7")
	visible := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(long.Get("text"), ""))
	if len(utf16.Encode([]rune(visible))) > 4096 || !strings.Contains(visible, "…") || !strings.Contains(long.Get("text"), `href="https://media.example/long.jpg"`) || long.Get("media") != "" {
		t.Fatalf("long post: %d units, %s", len(utf16.Encode([]rune(visible))), long.Get("text"))
	}
	partial := choose(1, "https://x.com/user/status/8")
	if !strings.Contains(partial.Get("text"), "Still here") || !strings.Contains(partial.Get("text"), "…") {
		t.Fatalf("inaccessible parent: %v", partial)
	}
}
