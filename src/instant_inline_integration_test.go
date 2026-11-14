package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type instantTelegramView struct {
	text, markup, parseMode, callback string
	results                           []map[string]any
	mediaEdits                        int
}

type instantTelegram struct {
	t    *testing.T
	mu   sync.Mutex
	view instantTelegramView
}

func (api *instantTelegram) snapshot() instantTelegramView {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.view
}

func (api *instantTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	api.mu.Lock()
	defer api.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
	case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
		api.view.text, api.view.markup = r.FormValue("text"), r.FormValue("reply_markup")
		api.view.parseMode = r.FormValue("parse_mode")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43}}`))
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		api.view.callback = r.FormValue("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		api.view.results = nil
		if err := json.Unmarshal([]byte(r.FormValue("results")), &api.view.results); err != nil {
			api.t.Error(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	case r.URL.Path == "/u/status/777":
		response := `{"code":200,"tweet":{"author":{"name":"Artist"},` +
			`"media":{"photos":[{"url":"https://example.com/photo"}]}}}`
		_, _ = w.Write([]byte(response))
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":44,"photo":[{"file_id":"file"}]}}`))
	case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
		api.view.mediaEdits++
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	default:
		api.t.Errorf("unexpected request %s", r.URL.Path)
	}
}

type instantNetworkGuard struct {
	target   *url.URL
	base     http.RoundTripper
	attempts atomic.Int32
	selected atomic.Bool
}

func (guard *instantNetworkGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != guard.target.Host {
		guard.attempts.Add(1)
		if !guard.selected.Load() || req.URL.Host != "api.fxtwitter.com" {
			return nil, fmt.Errorf("unexpected upstream request to %s", req.URL.Host)
		}
		copy := req.Clone(req.Context())
		u := *req.URL
		u.Scheme, u.Host = guard.target.Scheme, guard.target.Host
		copy.URL = &u
		req = copy
	}
	return guard.base.RoundTrip(req)
}

type instantSettingsScenario struct {
	t       *testing.T
	db      *sql.DB
	bot     *tgbotapi.BotAPI
	api     *instantTelegram
	network *instantNetworkGuard
}

func (s *instantSettingsScenario) open() {
	s.t.Helper()
	message := &tgbotapi.Message{
		MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1}, Text: "/settings",
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Length: len("/settings")}},
	}
	handleMessage(s.bot, nil, s.db, message, nil)
}

func (s *instantSettingsScenario) buttons() []tgbotapi.InlineKeyboardButton {
	s.t.Helper()
	var keyboard tgbotapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(s.api.snapshot().markup), &keyboard); err != nil {
		s.t.Fatal(err)
	}
	var buttons []tgbotapi.InlineKeyboardButton
	for _, row := range keyboard.InlineKeyboard {
		buttons = append(buttons, row...)
	}
	return buttons
}

func (s *instantSettingsScenario) expectButtons(labels ...string) {
	s.t.Helper()
	var actual []string
	for _, button := range s.buttons() {
		actual = append(actual, button.Text)
	}
	if !reflect.DeepEqual(actual, labels) {
		s.t.Fatalf("buttons = %v, want %v", actual, labels)
	}
}

func (s *instantSettingsScenario) tap(label string, actor int64) {
	s.t.Helper()
	for _, button := range s.buttons() {
		if button.Text == label && button.CallbackData != nil {
			query := &tgbotapi.CallbackQuery{
				ID: "callback", From: &tgbotapi.User{ID: actor}, Data: *button.CallbackData,
				Message: &tgbotapi.Message{MessageID: 43, Chat: &tgbotapi.Chat{ID: 123}},
			}
			handleCallback(s.bot, s.db, query)
			return
		}
	}
	s.t.Fatalf("button %q not found", label)
}

func (s *instantSettingsScenario) inline(user int64, link string) map[string]any {
	s.t.Helper()
	before := s.network.attempts.Load()
	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: user}, Query: link}
	handleInlineQuery(s.bot, s.db, query, testPreviews(s.t, s.db, s.bot))
	if after := s.network.attempts.Load(); after != before {
		s.t.Fatalf("inline query attempted %d upstream requests", after-before)
	}
	results := s.api.snapshot().results
	if len(results) != 1 {
		s.t.Fatalf("inline results: %#v", results)
	}
	return results[0]
}

func (s *instantSettingsScenario) expectThumbnail(user int64, link string, withPreview bool) {
	s.t.Helper()
	result := s.inline(user, link)
	if withPreview {
		_, thumbnail := uncachedThumbnailURLs(s.t, result)
		parsed, err := url.Parse(thumbnail)
		if err != nil || parsed.Query().Get("url") != inlineLink(link).String() {
			s.t.Fatalf("thumbnail for %s: %q, %v", link, thumbnail, err)
		}
		return
	}
	want := "https://preview.example.com/thumbnail-placeholder-x.jpg"
	if strings.Contains(link, "pixiv.net") {
		want = "https://preview.example.com/thumbnail-placeholder-pixiv.jpg"
	}
	if result["type"] != "photo" || result["photo_url"] != want || result["thumbnail_url"] != want {
		s.t.Fatalf("placeholder for %s: %#v", link, result)
	}
}

func TestInstantInlineSettingsFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	api := &instantTelegram{t: t}
	server := httptest.NewServer(api)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	guard := &instantNetworkGuard{target: target, base: original}
	http.DefaultTransport = guard
	defer func() { _ = db.Close(); http.DefaultTransport = original; server.Close() }()
	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &instantSettingsScenario{t: t, db: db, bot: bot, api: api, network: guard}
	verifyInstantSettingsMenu(t, s)
	verifyInstantInlineQueries(t, s)
	verifyInstantInlineSelection(t, s)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db = db
	s.open()
	s.tap("Instant inline query", 1)
	s.expectButtons("Toggle all", "X: ✓", "Pixiv: ✓", "Back")
	s.expectThumbnail(1, "https://x.com/u/status/1", false)
	s.expectThumbnail(1, "https://pixiv.net/artworks/2", false)
	s.tap("Back", 1)
	if view := api.snapshot(); view.text != "Settings" || !strings.Contains(view.markup, "Configure history") {
		t.Fatalf("settings back: %#v", view)
	}
	if _, err := db.Exec(`DROP TABLE instant_inline_settings`); err != nil {
		t.Fatal(err)
	}
	s.expectThumbnail(1, "https://x.com/u/status/999", false)
}

func verifyInstantSettingsMenu(t *testing.T, s *instantSettingsScenario) {
	t.Helper()
	s.open()
	if view := s.api.snapshot(); view.text != "Settings" || !strings.Contains(view.markup, "Instant inline query") {
		t.Fatalf("settings: %#v", view)
	}
	s.tap("Instant inline query", 1)
	view := s.api.snapshot()
	if view.parseMode != "HTML" || !strings.Contains(view.text, "<blockquote>") || !strings.Contains(view.text, "<b>Instant inline query</b>") || !strings.Contains(view.text, "previews") {
		t.Fatalf("submenu: %#v", view)
	}
	s.expectButtons("Toggle all", "X: ✗", "Pixiv: ✗", "Back")
	s.tap("X: ✗", 2)
	if view := s.api.snapshot(); view.callback == "" {
		t.Fatal("another user could change settings")
	}
	s.expectButtons("Toggle all", "X: ✗", "Pixiv: ✗", "Back")
}

func verifyInstantInlineQueries(t *testing.T, s *instantSettingsScenario) {
	t.Helper()
	x := "https://x.com/u/status/1"
	pixiv := "https://www.pixiv.net/artworks/2"
	s.expectThumbnail(1, x, true)
	s.expectThumbnail(1, pixiv, true)
	s.tap("X: ✗", 1)
	s.expectButtons("Toggle all", "X: ✓", "Pixiv: ✗", "Back")
	for _, host := range []string{"x.com", "fixupx.com", "vxtwitter.com", "fixvx.com", "twitter.com", "fxtwitter.com"} {
		s.expectThumbnail(1, "https://"+host+"/u/status/1", false)
	}
	s.expectThumbnail(1, pixiv, true)
	s.expectThumbnail(2, x, true)

	s.tap("Toggle all", 1)
	s.expectButtons("Toggle all", "X: ✓", "Pixiv: ✓", "Back")
	s.expectThumbnail(1, pixiv, false)
	s.expectThumbnail(1, "https://pixiv.net/en/artworks/2", false)
	s.tap("X: ✓", 1)
	s.tap("Toggle all", 1)
	s.expectButtons("Toggle all", "X: ✓", "Pixiv: ✓", "Back")
	s.tap("Toggle all", 1)
	s.expectButtons("Toggle all", "X: ✗", "Pixiv: ✗", "Back")
	s.tap("Pixiv: ✗", 1)
	s.expectThumbnail(1, x, true)
	s.expectThumbnail(1, pixiv, false)
	s.tap("X: ✗", 1)
	s.expectButtons("Toggle all", "X: ✓", "Pixiv: ✓", "Back")
}

func verifyInstantInlineSelection(t *testing.T, s *instantSettingsScenario) {
	t.Helper()
	link := "https://x.com/u/status/777"
	s.expectThumbnail(1, link, false)
	s.network.selected.Store(true)
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "inline", Query: link,
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	handleChosenInlineResult(s.bot, nil, s.db, chosen, "@cache", queue)
	if attempts, edits := s.network.attempts.Load(), s.api.snapshot().mediaEdits; attempts != 1 || edits != 1 {
		t.Fatalf("selection: upstream requests %d, media edits %d", attempts, edits)
	}
	result := s.inline(1, link)
	if result["type"] != "photo" || !strings.HasPrefix(result["photo_url"].(string), "https://preview.example.com/thumbnail?") {
		t.Fatalf("cached result: %#v", result)
	}
}
