package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPostImageThemeSettingsFlow(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	path := filepath.Join(t.TempDir(), "settings.db")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var text, markup, resultsJSON string
	server := themeSettingsServer(t, &text, &markup, &resultsJSON)
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews, err := newThumbnails("https://preview.example.com", nil, bot, db, "@cache")
	if err != nil {
		t.Fatal(err)
	}

	message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 11}}
	verifyThemeSettingsMenu(t, bot, db, message, &text, &markup)
	var keyboard tgbotapi.InlineKeyboardMarkup

	query := func(user int64, q string) []map[string]any {
		t.Helper()
		handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: user}, Query: q}, previews)
		var results []map[string]any
		if err := json.Unmarshal([]byte(resultsJSON), &results); err != nil {
			t.Fatal(err)
		}
		return results
	}

	before := query(11, "theme light ")
	if len(before) != 2 || before[0]["id"] != "theme:light" || before[1]["id"] != "theme:warm" || before[1]["title"] != "Warm" || before[1]["input_message_content"].(map[string]any)["message_text"] != "Setting theme…" {
		t.Fatal(before)
	}
	verifyThemePreviewSelection(t, before, "theme:light")
	verifyThemePreviewSelection(t, query(11, "theme dark "), "theme:dark")
	if filtered := query(11, "THEME LIGHT WaRm"); len(filtered) != 1 || filtered[0]["id"] != "theme:warm" {
		t.Fatal(filtered)
	}

	choose := func(user int64, id string) {
		chosen := &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: user}, ResultID: "theme:" + id, InlineMessageID: "inline",
		}
		handleChosenInlineResult(bot, nil, db, chosen, "", nil, previews)
	}

	choose(11, "warm")
	if !strings.Contains(text, "Warm") {
		t.Fatal(text)
	}
	if err := json.Unmarshal([]byte(markup), &keyboard); err != nil {
		t.Fatal(err)
	}
	if !isThemeSelection(&tgbotapi.Message{ReplyMarkup: &keyboard}) {
		t.Fatal(markup)
	}
	selection := *message
	selection.ViaBot, selection.ReplyMarkup = &bot.Self, &keyboard
	handleIncomingMessage(bot, nil, db, &telegramMessage{Message: selection}, nil, previews)
	choose(11, "dim")
	choose(12, "dark")
	choose(11, "invalid")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}

	previews.db = db
	for user, want := range map[int64]postImageThemes{11: {"warm", "dim"}, 12: {"light", "dark"}, 13: {"light", "dark"}} {
		got, err := loadPostImageThemes(db, user)
		if err != nil || got != want {
			t.Fatal(user, got, err)
		}
	}

	verifyThemePreviewSelection(t, query(11, "theme light "), "theme:warm")
	verifyThemePreviewSelection(t, query(11, "theme dark "), "theme:dim")
	if count, err := historyCount(db, 11); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM artwork`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}

	verifySelectedPostImageResults(t, db, query)
}

func themeSettingsServer(t *testing.T, text, markup, results *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
			*text, *markup = r.FormValue("text"), r.FormValue("reply_markup")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":43}}`)
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			if r.FormValue("cache_time") != "0" || r.FormValue("is_personal") != "true" {
				t.Error(r.Form)
			}
			*results = r.FormValue("results")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
}

func verifyThemePreviewSelection(t *testing.T, results []map[string]any, selected string) {
	t.Helper()
	for _, result := range results {
		address, err := url.Parse(result["thumbnail_url"].(string))
		if err != nil || (address.Query().Get("selected") == "true") != (result["id"] == selected) {
			t.Fatalf("selected preview: %v, %v", result, err)
		}
		if result["photo_url"] == result["thumbnail_url"] || result["photo_width"].(float64) < 1000 || result["photo_width"] == result["photo_height"] {
			t.Fatalf("preview photo: %v", result)
		}
	}
}

func verifySelectedPostImageResults(t *testing.T, db *sql.DB, query func(int64, string) []map[string]any) {
	t.Helper()
	if err := toggleInstantInline(db, 11, "x"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, result := range query(11, "https://x.com/u/status/1") {
		if id := result["id"].(string); strings.HasPrefix(id, "post-image:") {
			ids = append(ids, id)
		}
	}
	if strings.Join(ids, ",") != "post-image:warm:1,post-image:dim:1,post-image:warm-bars:1,post-image:dim-bars:1" {
		t.Fatal(ids)
	}
}

func verifyThemeSettingsMenu(t *testing.T, bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message, text, markup *string) {
	t.Helper()
	showSettings(bot, db, message)
	var keyboard tgbotapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(*markup), &keyboard); err != nil {
		t.Fatal(err)
	}
	row := keyboard.InlineKeyboard
	if row[len(row)-1][0].Text != "Timezone" {
		t.Fatal(*markup)
	}
	var action string
	for _, r := range row {
		if r[0].Text == "Post image themes" {
			action = *r[0].CallbackData
		}
	}
	if action == "" {
		t.Fatal(*markup)
	}
	callback := &tgbotapi.CallbackQuery{ID: "callback", From: message.From, Data: action, Message: message}
	handleSettingsCallback(bot, db, callback)
	if *text != "Settings\n\nPost image themes" {
		t.Fatal(*text)
	}
	if err := json.Unmarshal([]byte(*markup), &keyboard); err != nil {
		t.Fatal(err)
	}
	if len(keyboard.InlineKeyboard) != 3 || *keyboard.InlineKeyboard[0][0].SwitchInlineQueryCurrentChat != "theme light " || *keyboard.InlineKeyboard[1][0].SwitchInlineQueryCurrentChat != "theme dark " {
		t.Fatal(*markup)
	}

}

func TestPostImageThemeAutomaticFallback(t *testing.T) {
	db := testArtworkDB(t)
	if err := savePostImageTheme(db, 1, "dim"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"1", "2"} {
		link := inlineLink("https://x.com/user/status/" + id)
		caption := `<a href="https://fxtwitter.com/user/status/` + id + `">Artist</a>`
		if err := savePostImage(db, link, "dim", defaultTimezone, defaultTranslationSettings(), "photo", "card", caption); err != nil {
			t.Fatal(err)
		}
	}

	var uploads, reused, edits, fetches int
	server := automaticPostImageServer(t, &uploads, &reused, &edits, &fetches)
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = xPostTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "api.vxtwitter.com" {
			return nil, fmt.Errorf("VxTwitter unavailable in fixture")
		}
		return (redirectTransport{target, original}).RoundTrip(request)
	})
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1}}
	if err := processLink(bot, nil, db, message, inlineLink("https://x.com/user/status/1"), nil); err != nil {
		t.Fatal(err)
	}
	chosen := &tgbotapi.ChosenInlineResult{
		From: message.From, ResultID: inlineResultID, InlineMessageID: "inline",
		Query: "https://x.com/user/status/2",
	}
	if err := editInlineArtwork(bot, nil, db, chosen, "@cache", nil); err != nil {
		t.Fatal(err)
	}
	if reused != 1 || edits != 1 || uploads != 0 {
		t.Fatalf("reused %d edits %d uploads %d", reused, edits, uploads)
	}
}
