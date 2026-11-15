package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type inlineActionServer struct {
	t         *testing.T
	mu        sync.Mutex
	question  url.Values
	answers   []string
	documents []url.Values
}

func (s *inlineActionServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":42,"from":{"id":11,"first_name":"user"},"chat":{"id":11,"type":"private"},"via_bot":{"id":1,"is_bot":true,"first_name":"bot"},
			"rich_message":{"blocks":[{"type":"slideshow","caption":{"text":["by ",{"type":"bold","text":{"type":"url","text":"Artist","url":"https://fxtwitter.com/artist/status/123"}}]}}]}}},
			{"update_id":2,"inline_query":{"id":"q","from":{"id":11,"first_name":"user"},"query":""}}
		]}`)
	case r.URL.Path == "/artist/status/123":
		response := `{"code":200,"tweet":{"author":{"name":"Artist"},"media":{"all":[` +
			`{"type":"photo","url":"https://pbs.twimg.com/media/art.jpg?name=large"}]}}}`
		_, _ = fmt.Fprint(w, response)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		s.mu.Lock()
		s.question = r.Form
		s.mu.Unlock()
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":43}}`)
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		s.mu.Lock()
		s.answers = append(s.answers, r.FormValue("text"))
		s.mu.Unlock()
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendDocument"):
		s.mu.Lock()
		s.documents = append(s.documents, r.Form)
		s.mu.Unlock()
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":44}}`)
	default:
		s.t.Errorf("unexpected request: %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestInlineArtworkActions(t *testing.T) {
	db := testArtworkDB(t)
	for _, user := range []int64{11, 12} {
		a := artwork{
			ID: "x:123", URL: "https://x.com/artist/status/123", Author: "Artist",
			Caption: "caption", Photos: []string{"photo"},
		}
		if err := saveSelection(db, user, a); err != nil {
			t.Fatal(err)
		}
	}

	api := &inlineActionServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	updates, err := getTelegramUpdates(bot, tgbotapi.NewUpdate(0))
	if err != nil || len(updates) != 2 || updates[0].Message == nil || updates[1].InlineQuery == nil {
		t.Fatalf("updates: %#v, %v", updates, err)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	handleIncomingMessage(bot, nil, db, updates[0].Message, queue, nil)
	question := api.question
	if question.Get("text") != "why did you send this to me?" || question.Get("reply_to_message_id") != "42" {
		t.Fatalf("question: %v", question)
	}
	var entities []tgbotapi.MessageEntity
	if raw := question.Get("entities"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &entities); err != nil {
			t.Fatal(err)
		}
	}
	if len(entities) != 0 {
		t.Fatalf("question unexpectedly contains links: %v", entities)
	}
	var keyboard tgbotapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(question.Get("reply_markup")), &keyboard); err != nil || len(keyboard.InlineKeyboard) != 2 {
		t.Fatalf("keyboard: %s, %v", question.Get("reply_markup"), err)
	}
	remove, originalButton := keyboard.InlineKeyboard[0][0], keyboard.InlineKeyboard[1][0]
	if remove.Text != "remove from the history" || originalButton.Text != "get full-res version" {
		t.Fatalf("buttons: %+v, %+v", remove, originalButton)
	}
	if len(*remove.CallbackData) > 64 || len(*originalButton.CallbackData) > 64 {
		t.Fatalf("callback data exceeds Telegram's limit: %s, %s", *remove.CallbackData, *originalButton.CallbackData)
	}
	callback := func(user int64, data string) {
		t.Helper()
		query := &tgbotapi.CallbackQuery{ID: "cb", From: &tgbotapi.User{ID: user}, Data: data,
			Message: &tgbotapi.Message{MessageID: 43, Chat: &tgbotapi.Chat{ID: 11}, Text: question.Get("text"), Entities: entities}}
		handleIncomingCallback(bot, nil, db, query, queue)
	}
	callback(12, *remove.CallbackData)
	callback(11, *remove.CallbackData)
	callback(11, *remove.CallbackData)
	if strings.Join(api.answers, "|") != "This button belongs to another user.|Removed from your history.|This link is not in your history." {
		t.Fatalf("answers: %v", api.answers)
	}
	for user, want := range map[int64]int{11: 0, 12: 1} {
		history, err := artworkHistory(db, user, 0, "")
		if err != nil || len(history) != want {
			t.Fatalf("history for %d: %v, %v", user, history, err)
		}
	}
	if saved, err := findArtwork(db, "x:123"); err != nil || saved == nil {
		t.Fatalf("shared artwork removed: %v, %v", saved, err)
	}

	// Original delivery remains available after deleting the history entry.
	callback(11, *originalButton.CallbackData)
	if len(api.documents) != 1 || api.documents[0].Get("document") != "https://pbs.twimg.com/media/art.jpg?name=orig" || api.documents[0].Get("reply_to_message_id") != "43" {
		t.Fatalf("documents: %v", api.documents)
	}

	// Previously sent questions stored the source as a text-link entity.
	legacy := &tgbotapi.CallbackQuery{ID: "legacy", From: &tgbotapi.User{ID: 12}, Data: "art:remove:12",
		Message: &tgbotapi.Message{MessageID: 44, Chat: &tgbotapi.Chat{ID: 12}, Text: "old question",
			Entities: []tgbotapi.MessageEntity{{Type: "text_link", Offset: 0, Length: 12, URL: "https://x.com/artist/status/123"}}}}
	handleIncomingCallback(bot, nil, db, legacy, queue)
	if api.answers[len(api.answers)-1] != "Removed from your history." {
		t.Fatalf("legacy callback: %v", api.answers)
	}
}

func TestInlineArtworkLoadingAndCaption(t *testing.T) {
	db := testArtworkDB(t)
	bot := &tgbotapi.BotAPI{}
	link := inlineLink("https://www.pixiv.net/artworks/123")
	result := inlineArtworkResult(db, link, testPreviews(t, db, bot), false)
	content := result["input_message_content"].(map[string]any)
	encoded, err := json.Marshal(content["entities"])
	if err != nil {
		t.Fatal(err)
	}
	var entities []tgbotapi.MessageEntity
	if err := json.Unmarshal(encoded, &entities); err != nil {
		t.Fatal(err)
	}
	loading := &telegramMessage{}
	loading.Text, loading.Entities = content["message_text"].(string), entities
	caption := &telegramMessage{}
	caption.Caption = "Artist"
	caption.CaptionEntities = []tgbotapi.MessageEntity{{Type: "text_link", Offset: 0, Length: 6, URL: link.String()}}
	for _, message := range []*telegramMessage{loading, caption} {
		if got := inlineMessageArtwork(message); got == nil || got.String() != link.String() {
			t.Fatalf("artwork link: %v", got)
		}
	}
	if err := setHistoryEnabled(db, 11, false); err != nil {
		t.Fatal(err)
	}
	query := &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: 11}, Data: artworkActionData("remove", 11, link),
		Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 11}, Text: "why did you send this to me?"}}
	text, job := startArtworkAction(bot, nil, db, query, nil)
	if text != "This link is not in your history." || job != nil {
		t.Fatalf("history-disabled Pixiv callback: %q, %v", text, job)
	}

	// Maximum IDs and an X handle still fit Telegram's 64-byte callback limit.
	x := inlineLink("https://x.com/abcdefghijklmno/status/18446744073709551615")
	query.From.ID = 9223372036854775807
	query.Data = artworkActionData("original", query.From.ID, x)
	if len(query.Data) > 64 {
		t.Fatalf("callback is %d bytes: %s", len(query.Data), query.Data)
	}
	decoded := artworkActionLink(strings.Split(query.Data, ":"))
	if decoded == nil || decoded.String() != x.String() {
		t.Fatalf("callback artwork: %v", decoded)
	}
}
