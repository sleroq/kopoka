package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestChatJSONCommand(t *testing.T) {
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
	handleMessage(bot, nil, &message)
	if strings.Join(requests, ",") != "getChat,sendMessage" {
		t.Fatalf("requests: %v", requests)
	}
}

func TestChatFormattedLinks(t *testing.T) {
	var fetched, sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/status/"):
			fetched = append(fetched, r.URL.RequestURI())
			response := `{"code":200,"tweet":{"author":{"name":"Artist"},` +
				`"media":{"photos":[{"url":"https://example.com/photo"}]}}}`
			_, _ = w.Write([]byte(response))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
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
	handleMessage(bot, nil, message)
	if strings.Join(fetched, ",") != "/user/status/1,/user/status/2,/user/status/3,/user/status/4" {
		t.Fatalf("fetched: %v", fetched)
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
