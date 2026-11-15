package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestChatJSONCommand(t *testing.T) {
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"kopoka_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getChat"):
			requests = append(requests, "getChat")
			if r.FormValue("chat_id") != "-100123" {
				t.Errorf("chat ID = %s", r.FormValue("chat_id"))
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":-100123,"type":"channel","new_api_field":"<hello>"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			requests = append(requests, "sendMessage")
			if r.FormValue("reply_to_message_id") != "42" || r.FormValue("parse_mode") != "HTML" || !strings.Contains(r.FormValue("text"), `&lt;hello&gt;`) || !strings.Contains(r.FormValue("text"), `&#34;id&#34;: -100123`) {
				t.Errorf("message: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	var message tgbotapi.Message
	update := `{"message_id":42,"chat":{"id":-100123,"type":"channel"},
		"text":"/json@kopoka_bot","entities":[{"type":"bot_command","offset":0,"length":16}]}`
	if err := json.Unmarshal([]byte(update), &message); err != nil {
		t.Fatal(err)
	}
	handleMessage(bot, nil, nil, &message, queue, nil)
	if strings.Join(requests, ",") != "getChat,sendMessage" {
		t.Fatalf("requests: %v", requests)
	}
}

func TestChatFormattedLinks(t *testing.T) {
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})

	var fetched, sent, actions []string
	var reactions int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/status/"):
			fetched = append(fetched, r.URL.RequestURI())
			response := `{"code":200,"tweet":{"author":{"name":"Artist"},` +
				`"media":{"photos":[{"url":"https://example.com/photo"}]}}}`
			_, _ = w.Write([]byte(response))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/setMessageReaction"):
			_ = r.ParseForm()
			var reaction []struct {
				Type  string `json:"type"`
				Emoji string `json:"emoji"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("reaction")), &reaction); err != nil || len(reaction) != 1 || reaction[0].Type != "emoji" || !slices.Contains([]string{"❤", "🥰", "🍓"}, reaction[0].Emoji) {
				t.Errorf("reaction: %s", r.FormValue("reaction"))
			}
			if r.FormValue("chat_id") != "123" || r.FormValue("message_id") != "42" || len(fetched) != 0 {
				t.Errorf("reaction request: %v, fetched: %v", r.Form, fetched)
			}
			reactions++
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			_ = r.ParseForm()
			actions = append(actions, r.FormValue("action"))
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_ = r.ParseForm()
			sent = append(sent, r.FormValue("caption"))
			if r.FormValue("chat_id") != "123" || r.FormValue("reply_to_message_id") != "42" {
				t.Errorf("photo: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	text := "😀 label https://x.com/user/status/2?tracking=1 " +
		"code: (https://x.com/user/status/3?ref=1). pre: https://x.com/user/status/4!"
	entity := func(kind, part string) tgbotapi.MessageEntity {
		start := strings.Index(text, part)
		return tgbotapi.MessageEntity{
			Type: kind, Offset: len(utf16.Encode([]rune(text[:start]))),
			Length: len(utf16.Encode([]rune(part))),
		}
	}
	hidden := entity("text_link", "label")
	hidden.URL = "https://x.com/user/status/1?tracking=1"
	message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{UserName: "user"}, Text: text,
		Entities: []tgbotapi.MessageEntity{
			hidden,
			entity("bold", "https://x.com/user/status/2?tracking=1"),
			entity("url", "https://x.com/user/status/2?tracking=1"),
			entity("code", "(https://x.com/user/status/3?ref=1)."),
			entity("pre", "https://x.com/user/status/4!"),
			entity("url", "https://x.com/user/status/2?tracking=1"),
			{Type: "url", Offset: 999, Length: 10},
		}}
	handleMessage(bot, nil, nil, message, queue, nil)
	if reactions != 1 {
		t.Fatalf("reactions: %d", reactions)
	}
	if strings.Join(fetched, ",") != "/user/status/1,/user/status/2,/user/status/3,/user/status/4" {
		t.Fatalf("fetched: %v", fetched)
	}
	if strings.Join(actions, ",") != "typing,upload_photo,typing,upload_photo,typing,upload_photo,typing,upload_photo" {
		t.Fatalf("chat actions: %v", actions)
	}
	if len(sent) != 4 {
		t.Fatalf("sent: %v", sent)
	}
	for i, caption := range sent {
		if !strings.Contains(caption, fetched[i]) || strings.Contains(caption, "tracking") || strings.Contains(caption, "?ref=") {
			t.Errorf("caption: %s", caption)
		}
	}
}

func TestChatHistorySettingsFlow(t *testing.T) {
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})

	path := filepath.Join(t.TempDir(), "history.db")
	// A database created before origins existed must retain inline history.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE artwork (id TEXT PRIMARY KEY, url TEXT NOT NULL, author TEXT NOT NULL, text TEXT NOT NULL, caption TEXT NOT NULL, photos TEXT NOT NULL);
		CREATE TABLE history (sequence INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, artwork_id TEXT NOT NULL, UNIQUE(user_id, artwork_id));
		INSERT INTO artwork VALUES ('x:99','https://x.com/u/status/99','Old','text','caption','["old"]');
		INSERT INTO history(user_id,artwork_id) VALUES (1,'x:99');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var answers []map[string]any
	var reply, markup, callback string
	server := chatHistoryServer(t, &answers, &reply, &markup, &callback)
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	chat := func(link string) {
		t.Helper()
		message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1}, Text: link,
			Entities: []tgbotapi.MessageEntity{{Type: "url", Length: len(link)}}}
		handleMessage(bot, nil, db, message, queue, nil)
	}
	command := func(text string) {
		message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1}, Text: text,
			Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Length: len(text)}}}
		handleMessage(bot, nil, db, message, queue, nil)
	}
	toggle := func(user int64, action string) {
		callback = "pending"
		query := &tgbotapi.CallbackQuery{ID: "cb", From: &tgbotapi.User{ID: user}, Data: "settings:" + action + ":1",
			Message: &tgbotapi.Message{MessageID: 43, Chat: &tgbotapi.Chat{ID: 123}}}
		handleCallback(bot, db, query)
	}
	check := func(ids ...string) {
		t.Helper()
		handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}}, testPreviews(t, db, bot))
		if len(answers) != len(ids) {
			t.Fatalf("history: %v want %v", answers, ids)
		}
		for i, id := range ids {
			if answers[i]["id"] != id {
				t.Fatalf("history: %v want %v", answers, ids)
			}
		}
	}

	chat("https://x.com/u/status/1?tracking=yes")
	a, err := findArtwork(db, "x:1")
	if err != nil || a == nil || a.URL != "https://x.com/u/status/1" || a.Author != "Artist" || a.Text != "Title" || a.Photos[0] != "usable" || !strings.Contains(a.Caption, "fxtwitter.com") {
		t.Fatalf("saved artwork: %#v, %v", a, err)
	}
	chat("https://x.com/u/status/3")
	chat("https://x.com/u/status/4")
	chat("https://x.com/u/status/invalid")
	check("x:1", "x:99")
	command("/settings")
	toggle(1, "history-menu")
	if reply != "Settings\n\nHistory" || !strings.Contains(markup, "Show chat links in search history: ✓") {
		t.Fatalf("settings: %q %s", reply, markup)
	}
	toggle(2, "chat")
	if callback == "" {
		t.Fatal("unauthorized callback accepted")
	}
	check("x:1", "x:99")
	toggle(1, "chat")
	check("x:99")
	chat("https://x.com/u/status/2")
	check("x:99")
	chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1}, ResultID: "x:1"}
	handleChosenInlineResult(bot, nil, db, chosen, "", queue, nil)
	chat("https://x.com/u/status/1")
	check("x:1", "x:99")
	command("/historyoff")
	chat("https://x.com/u/status/5")
	check("x:1", "x:99")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	command("/settings")
	toggle(1, "history-menu")
	if reply != "Settings\n\nHistory" || !strings.Contains(markup, "Enable history") || !strings.Contains(markup, "Show chat links in search history: ✗") {
		t.Fatalf("settings: %q %s", reply, markup)
	}
	toggle(1, "chat")
	check("x:1", "x:2", "x:99")
	toggle(1, "enable-history")
	chat("https://x.com/u/status/5")
	check("x:5", "x:1", "x:2", "x:99")
	toggle(1, "order")
	check("x:5", "x:2", "x:1", "x:99")
	toggle(1, "order")
	check("x:5", "x:1", "x:2", "x:99")
}

func chatHistoryServer(t *testing.T, answers *[]map[string]any, reply, markup, callback *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasPrefix(r.URL.Path, "/u/status/"):
			if strings.HasSuffix(r.URL.Path, "/4") {
				_, _ = w.Write([]byte(`{"code":200,"tweet":{"author":{"name":"Artist"},"media":{"photos":[]}}}`))
				return
			}
			response := `{"code":200,"tweet":{"text":"Title","author":{"name":"Artist"},` +
				`"media":{"photos":[{"url":"https://example.com/photo"}]}}}`
			_, _ = w.Write([]byte(response))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/setMessageReaction"), strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			if strings.Contains(r.FormValue("caption"), "/status/3") {
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43,"photo":[{"file_id":"small"},{"file_id":"usable"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
			*reply = r.FormValue("text")
			*markup = r.FormValue("reply_markup")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43}}`))
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			*callback = r.FormValue("text")
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			if err := json.Unmarshal([]byte(r.FormValue("results")), answers); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
}
