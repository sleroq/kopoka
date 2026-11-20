package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTimezoneInlineRouting(t *testing.T) {
	var results []struct {
		ID          string                         `json:"id"`
		ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
	}
	var next, confirmation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			if err := json.Unmarshal([]byte(r.FormValue("results")), &results); err != nil {
				t.Error(err)
			}
			next = r.FormValue("next_offset")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			if r.FormValue("inline_message_id") != "timezone-inline" {
				t.Errorf("wrong edit target: %v", r.Form)
			}
			confirmation = r.FormValue("text")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"unexpected request"}`)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	db := testArtworkDB(t)
	user := &tgbotapi.User{ID: 7}
	query := &tgbotapi.InlineQuery{ID: "query", From: user, Query: "tz "}
	handleInlineQuery(time.Now(), bot, db, query, nil)
	if len(results) != 50 || next == "" {
		t.Fatalf("timezone page: %d results, next %q", len(results), next)
	}
	query.Query = "tZ gErMaNy"
	handleInlineQuery(time.Now(), bot, db, query, nil)
	var markup *tgbotapi.InlineKeyboardMarkup
	for _, result := range results {
		if result.ID == "timezone:Europe/Berlin" {
			markup = result.ReplyMarkup
		}
	}
	if markup == nil {
		t.Fatal("country search omitted Berlin or its inline keyboard")
	}
	chosen := &tgbotapi.ChosenInlineResult{
		From: user, ResultID: "timezone:Europe/Berlin", Query: query.Query, InlineMessageID: "timezone-inline",
	}
	handleChosenInlineResult(time.Now(), bot, nil, db, chosen, "", nil, nil)
	zone, err := loadUserTimezone(db, user.ID)
	if err != nil || zone != "Europe/Berlin" || !strings.Contains(confirmation, "Europe/Berlin") {
		t.Fatalf("selection: zone %q, confirmation %q, error %v", zone, confirmation, err)
	}
	zone, err = loadUserTimezone(db, user.ID+1)
	if err != nil || zone != defaultTimezone {
		t.Fatalf("other user: %q, %v", zone, err)
	}
	incoming := &telegramMessage{Message: tgbotapi.Message{
		MessageID: 42, From: user, Chat: &tgbotapi.Chat{ID: user.ID}, ViaBot: &bot.Self,
		Text: "Setting timezone…", ReplyMarkup: markup,
	}}
	handleIncomingMessage(bot, nil, db, incoming, nil, nil)
	var history, artwork int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM history), (SELECT count(*) FROM artwork)`).Scan(&history, &artwork); err != nil {
		t.Fatal(err)
	}
	if history != 0 || artwork != 0 {
		t.Fatalf("timezone selection changed artwork/history: %d/%d", artwork, history)
	}
}
