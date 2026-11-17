package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTranslationSettingsIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}

	var results []struct {
		ID          string                         `json:"id"`
		Title       string                         `json:"title"`
		Description string                         `json:"description"`
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

	user := &tgbotapi.User{ID: 7}
	settings, err := loadTranslationSettings(db, user.ID)
	if err != nil || settings.Target != "en" || !slices.Equal(settings.Ignored, []string{"ru"}) || !settings.ignores("RU-ru") {
		t.Fatal(settings, err)
	}
	sorted := translationSettings{Target: "en", Ignored: []string{"ru", "ja"}}
	if sorted.key() != "en:ja,ru" || sorted.Ignored[0] != "ru" || languageName("zh-cn") != "Chinese" {
		t.Fatal(sorted)
	}

	_, keyboard := settingsView(user.ID)
	buttons := map[string]string{}
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if button.SwitchInlineQueryCurrentChat != nil {
				buttons[button.Text] = *button.SwitchInlineQueryCurrentChat
			}
		}
	}
	if buttons["Target language"] != "language target " || buttons["Ignore translations"] != "language ignore " {
		t.Fatal(buttons)
	}

	query := &tgbotapi.InlineQuery{ID: "query", From: user, Query: "language target "}
	handleInlineQuery(bot, db, query, nil)
	if len(results) != 50 || next == "" {
		t.Fatalf("page: %d, %q", len(results), next)
	}
	firstID := results[0].ID
	query.Offset = next
	handleInlineQuery(bot, db, query, nil)
	if len(results) == 0 || results[0].ID == firstID {
		t.Fatal("invalid second page")
	}
	query.Offset = ""
	query.Query = "LaNgUaGe target jApAnEsE"
	handleInlineQuery(bot, db, query, nil)
	if len(results) != 1 || results[0].ID != "language:target:ja" || !strings.Contains(results[0].Description, "English") {
		t.Fatal(results)
	}
	markup := results[0].ReplyMarkup
	chosen := &tgbotapi.ChosenInlineResult{
		From: user, ResultID: results[0].ID, Query: query.Query, InlineMessageID: "inline",
	}
	handleChosenInlineResult(bot, nil, db, chosen, "", nil, nil)
	if !strings.Contains(confirmation, "Japanese") {
		t.Fatal(confirmation)
	}
	query.Query = "language ignore russian"
	handleInlineQuery(bot, db, query, nil)
	if len(results) != 1 || results[0].ID != "language:ignore:ru" || !strings.Contains(results[0].Title, "✓") || results[0].Description != "Translate" {
		t.Fatal(results)
	}
	chosen.ResultID = results[0].ID
	handleChosenInlineResult(bot, nil, db, chosen, "", nil, nil)
	if !strings.Contains(confirmation, "None") {
		t.Fatal(confirmation)
	}
	query.Query = "language ignore russian"
	handleInlineQuery(bot, db, query, nil)
	if results[0].Description != "Ignore" {
		t.Fatal(results)
	}

	incoming := &telegramMessage{Message: tgbotapi.Message{
		MessageID: 42, From: user, Chat: &tgbotapi.Chat{ID: user.ID}, ViaBot: &bot.Self, ReplyMarkup: markup,
	}}
	handleIncomingMessage(bot, nil, db, incoming, nil, nil)
	chosen.ResultID = "language:target:invalid"
	handleChosenInlineResult(bot, nil, db, chosen, "", nil, nil)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	settings, err = loadTranslationSettings(db, user.ID)
	if err != nil || settings.Target != "ja" || len(settings.Ignored) != 0 || settings.Ignored == nil {
		t.Fatal(settings, err)
	}
	other, err := loadTranslationSettings(db, user.ID+1)
	if err != nil || other.Target != "en" || !slices.Equal(other.Ignored, []string{"ru"}) {
		t.Fatal(other, err)
	}
	if count, err := historyCount(db, user.ID); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
