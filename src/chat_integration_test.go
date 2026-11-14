package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
