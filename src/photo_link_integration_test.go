package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type photoLinkServer struct {
	t                          *testing.T
	captions, documents, paths []string
	inlineResults              string
	reply, edit, markup        string
	fullPost                   bool
}

func (api *photoLinkServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case (r.URL.Path == "/artist/status/123" || r.URL.Path == "/artist/status/123/en"):
		api.paths = append(api.paths, r.URL.RequestURI())
		if api.fullPost {
			_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"author":{"name":"Artist"},"media":{"all":[`+
				`{"type":"video","url":"https://example.com/video"},`+
				`{"type":"photo","url":"https://pbs.twimg.com/media/second.jpg?name=large"}]}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"author":{"name":"Artist"},"media":{"all":[`+
			`{"type":"photo"},{"type":"photo","url":"https://pbs.twimg.com/media/second.jpg?name=large"},`+
			`{"type":"video","url":"https://example.com/video"}]}}}`)
	case strings.HasSuffix(r.URL.Path, "/sendChatAction"), strings.HasSuffix(r.URL.Path, "/setMessageReaction"), strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		api.edit, api.markup = r.FormValue("text"), r.FormValue("reply_markup")
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		api.reply = r.FormValue("text")
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":43}}`)
	case strings.HasSuffix(r.URL.Path, "/sendMediaGroup"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":[{"message_id":41,"video":{"file_id":"vid","width":1,"height":1,"duration":1}},`+
			`{"message_id":42,"photo":[{"file_id":"photo2","width":1,"height":1}]}]}`)
	case strings.HasSuffix(r.URL.Path, "/sendVideo"):
		video := `{"file_id":"vid","width":1,"height":1,"duration":1}`
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":41,"video":`+video+`}}`)
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
		api.captions = append(api.captions, r.FormValue("caption"))
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"photo":[{"file_id":"photo2","width":1,"height":1}]}}`)
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		api.inlineResults = r.FormValue("results")
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
	case strings.HasSuffix(r.URL.Path, "/sendDocument"):
		api.documents = append(api.documents, r.FormValue("document"))
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":43}}`)
	default:
		api.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	}
}

func startPhotoLinkServer(t *testing.T) (*photoLinkServer, *tgbotapi.BotAPI) {
	t.Helper()
	api := &photoLinkServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	old := http.DefaultTransport
	http.DefaultTransport = xPostTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.vxtwitter.com" {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
				Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header), Request: r,
			}, nil
		}
		return (redirectTransport{target, old}).RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	return api, bot
}

func TestPhotoLinkFlow(t *testing.T) {
	db := testArtworkDB(t)
	api, bot := startPhotoLinkServer(t)
	message := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 11}, From: &tgbotapi.User{ID: 11}}
	chosen, previews := saveSelectedPhoto(t, db, bot, message, api)
	bad := inlineLink("https://x.com/artist/status/123/photo/3")
	var unavailable unavailablePhotoError
	if err := processLink(bot, nil, db, message, bad, nil); !errors.As(err, &unavailable) || unavailable.index != 3 {
		t.Fatalf("invalid selection: %v", err)
	}
	outOfRange := inlineLink("https://x.com/artist/status/123/photo/9")
	if err := processLink(bot, nil, db, message, outOfRange, nil); !errors.As(err, &unavailable) || unavailable.index != 9 {
		t.Fatalf("out of range: %v", err)
	}
	callbackLink := inlineLink("https://fxtwitter.com/artist/status/123/photo/2")
	data := artworkActionData("original", 11, callbackLink)
	restored := artworkActionLink(strings.Split(data, ":"))
	if len(data) > 64 || restored == nil || xPhotoIndex(restored) != 2 {
		t.Fatalf("callback %q: %v", data, restored)
	}
	handleSettingsCallback(bot, db, &tgbotapi.CallbackQuery{ID: "settings", From: message.From,
		Message: message, Data: "settings:photo-toggle:11"})
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	originalCallback := &tgbotapi.CallbackQuery{ID: "original", From: message.From, Message: message, Data: data}
	handleArtworkCallback(bot, nil, db, originalCallback, queue)
	if len(api.documents) != 1 || !strings.Contains(api.documents[0], "second.jpg?name=orig") {
		t.Fatalf("documents: %v", api.documents)
	}
	preferred, err := preferredPhotoLink(db, 11, callbackLink)
	if err != nil || artworkID(preferred) != "x:123" {
		t.Fatalf("preference: %v %v", preferred, err)
	}

	// With the preference disabled, both chat and inline save the parent identity.
	api.fullPost = true
	offLink := inlineLink("https://x.com/artist/status/123/photo/2")
	if err := processLink(bot, nil, db, message, offLink, nil); err != nil {
		t.Fatal(err)
	}
	parent, err := findArtwork(db, "x:123")
	if err != nil || parent == nil || len(parent.Photos) != 2 || strings.Contains(parent.Caption, "/photo/") {
		t.Fatalf("parent: %+v %v", parent, err)
	}
	if err := editInlineArtwork(bot, nil, db, chosen, "11", nil); err != nil {
		t.Fatal(err)
	}
	if err := saveThumbnail(db, "x:123", 2, 2, "thumb"); err != nil {
		t.Fatal(err)
	}
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: message.From, Query: chosen.Query}, previews)
	if !strings.Contains(api.inlineResults, `"id":"x:123"`) {
		t.Fatalf("inline result: %s", api.inlineResults)
	}
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 12}, Query: chosen.Query}, previews)
	if !strings.Contains(api.inlineResults, `"id":"x:123:photo:2"`) {
		t.Fatalf("other user's selected inline result: %s", api.inlineResults)
	}
	if selected, _ := findArtwork(db, "x:123:photo:2"); selected == nil {
		t.Fatal("parent overwrote selected photo")
	}
	remove := artworkActionData("remove", 11, callbackLink)
	removeCallback := &tgbotapi.CallbackQuery{ID: "remove", From: message.From, Message: message, Data: remove}
	handleArtworkCallback(bot, nil, db, removeCallback, queue)
	history, err := artworkHistory(db, 11, 0, "")
	if err != nil || len(history) != 1 || history[0].ID != "x:123" {
		t.Fatalf("history after removal: %+v %v", history, err)
	}
	if len(api.paths) < 3 {
		t.Fatal(api.paths)
	}
}

func TestUnavailablePhotoLinkReply(t *testing.T) {
	db := testArtworkDB(t)
	api, bot := startPhotoLinkServer(t)
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	link := "https://x.com/artist/status/123/photo/9"
	user := &tgbotapi.User{ID: 11}
	message := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 11}, From: user, Text: link,
		Entities: []tgbotapi.MessageEntity{{Type: "url", Length: len(link)}}}
	handleMessage(bot, nil, db, message, queue, nil)
	if !strings.Contains(api.reply, "photo 9 is unavailable") {
		t.Fatalf("chat error: %q", api.reply)
	}

	chosen := &tgbotapi.ChosenInlineResult{From: user, ResultID: inlineResultID, InlineMessageID: "inline", Query: link}
	handleChosenInlineResult(bot, nil, db, chosen, "11", queue, nil)
	if !strings.Contains(api.edit, "photo 9 is unavailable") {
		t.Fatalf("inline error: %q", api.edit)
	}
}

func TestPhotoLinkSettings(t *testing.T) {
	db := testArtworkDB(t)
	api, bot := startPhotoLinkServer(t)
	user := &tgbotapi.User{ID: 11}
	message := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 11}, From: user}
	for _, step := range []struct{ action, label string }{
		{"photo-menu", "Enabled"}, {"photo-toggle", "Disabled"}, {"photo-toggle", "Enabled"},
	} {
		handleSettingsCallback(bot, db, &tgbotapi.CallbackQuery{
			ID: "settings", From: user, Message: message, Data: "settings:" + step.action + ":11",
		})
		if !strings.Contains(api.markup, step.label) {
			t.Fatalf("%s: %s", step.action, api.markup)
		}
	}
}

func saveSelectedPhoto(t *testing.T, db *sql.DB, bot *tgbotapi.BotAPI, message *tgbotapi.Message, api *photoLinkServer) (*tgbotapi.ChosenInlineResult, *thumbnails) {
	t.Helper()
	link := inlineLink("https://x.com/artist/status/123/photo/002?foo=1#frag")
	if link == nil || link.Path != "/artist/status/123/photo/2" {
		t.Fatalf("normalized link: %v", link)
	}
	if err := processLink(bot, nil, db, message, link, nil); err != nil {
		t.Fatal(err)
	}

	a, err := findArtwork(db, "x:123:photo:2")
	if err != nil || a == nil || !strings.Contains(a.Caption, "fxtwitter.com/artist/status/123/photo/2") || !strings.Contains(a.Caption, "Artist") {
		t.Fatalf("selected artwork: %+v %v", a, err)
	}
	if a.URL != "https://x.com/artist/status/123/photo/2" {
		t.Fatal(a.URL)
	}
	if len(api.captions) != 1 || !strings.Contains(api.captions[0], "/photo/2") {
		t.Fatal(api.captions)
	}
	if parent, _ := findArtwork(db, "x:123"); parent != nil {
		t.Fatal("selected photo overwrote post")
	}

	if err := saveThumbnail(db, "x:123:photo:2", 1, 1, "selected-thumb"); err != nil {
		t.Fatal(err)
	}
	previews, err := newThumbnails("https://preview.example.com", nil, bot, db, "11")
	if err != nil {
		t.Fatal(err)
	}
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{
		ID: "q", From: message.From, Query: "https://x.com/artist/status/123/photo/2",
	}, previews)
	if !strings.Contains(api.inlineResults, `"id":"x:123:photo:2"`) || !strings.Contains(api.inlineResults, `"photo_file_id":"selected-thumb"`) {
		t.Fatalf("selected inline result: %s", api.inlineResults)
	}

	chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 11},
		Query: "https://x.com/artist/status/123/photo/2/", InlineMessageID: "inline"}
	if err := editInlineArtwork(bot, nil, db, chosen, "11", nil); err != nil {
		t.Fatal(err)
	}
	return chosen, previews
}

func TestInlineNumericPhotoSelection(t *testing.T) {
	db := testArtworkDB(t)
	api, bot := startPhotoLinkServer(t)
	api.fullPost = true
	user := &tgbotapi.User{ID: 11}
	if err := togglePhotoLinks(db, user.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{11, 12} {
		if err := toggleInstantInline(db, id, "x"); err != nil {
			t.Fatal(err)
		}
	}
	previews := testPreviews(t, db, bot)
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	query := "look https://x.com/artist/status/123/photo/1 2 now"
	chosen := &tgbotapi.ChosenInlineResult{From: user, ResultID: inlineResultID, InlineMessageID: "inline", Query: query}
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: user, Query: query}, previews)
	if !strings.Contains(api.inlineResults, `"id":"artwork"`) || strings.Contains(api.inlineResults, `artwork:merged`) {
		t.Fatal(api.inlineResults)
	}
	previews.mu.Lock()
	workCount := len(previews.postImageWork)
	previews.mu.Unlock()
	if workCount != 0 {
		t.Fatalf("explicit selection started %d post-image previews", workCount)
	}

	handleChosenInlineResult(bot, nil, db, chosen, "11", queue, nil)
	selected, err := findArtwork(db, "x:123:photo:2")
	if err != nil || selected == nil || len(selected.Photos) != 1 {
		t.Fatalf("selected: %+v %v", selected, err)
	}
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: user, Query: query}, previews)
	if !strings.Contains(api.inlineResults, `"id":"x:123:photo:2"`) || strings.Contains(api.inlineResults, `artwork:merged`) {
		t.Fatal(api.inlineResults)
	}
	chosen.ResultID = selected.ID
	handleChosenInlineResult(bot, nil, db, chosen, "11", queue, nil)
	chosen.ResultID, chosen.Query = inlineResultID, "https://x.com/artist/status/123 9"
	handleChosenInlineResult(bot, nil, db, chosen, "11", queue, nil)
	if !strings.Contains(api.edit, "media 9 is unavailable") {
		t.Fatal(api.edit)
	}

	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{
		ID: "q", From: &tgbotapi.User{ID: 12}, Query: "https://x.com/artist/status/123/photo/2",
	}, previews)
	if !strings.Contains(api.inlineResults, "Post image") {
		t.Fatalf("photo link without explicit selection lost post-image results: %s", api.inlineResults)
	}
}

func TestSelectedXVideoFlow(t *testing.T) {
	db := testArtworkDB(t)
	api, bot := startPhotoLinkServer(t)

	api.fullPost = true
	user := &tgbotapi.User{ID: 11}
	message := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 11}, From: user}
	if err := togglePhotoLinks(db, user.ID); err != nil {
		t.Fatal(err)
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	query := "https://x.com/artist/status/123 1"
	chosen := &tgbotapi.ChosenInlineResult{From: user, ResultID: inlineResultID, InlineMessageID: "inline", Query: query}
	handleChosenInlineResult(bot, nil, db, chosen, "11", queue, nil)
	selected, err := findArtwork(db, "x:123:photo:1")
	if err != nil || selected == nil || len(selected.Types) != 1 || selected.Types[0] != "video" || !strings.Contains(selected.Caption, "/video/1") || strings.Contains(selected.Caption, "/media/") {
		t.Fatalf("inline video: %+v %v", selected, err)
	}
	if err := saveThumbnail(db, selected.ID, 1, 1, "video-thumb"); err != nil {
		t.Fatal(err)
	}
	previews := testPreviews(t, db, bot)
	handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "q", From: user, Query: query}, previews)
	if !strings.Contains(api.inlineResults, `"id":"x:123:photo:1"`) || !strings.Contains(api.inlineResults, "video-thumb") {
		t.Fatal(api.inlineResults)
	}
	link := inlineLink("https://x.com/artist/status/123/video/001?ignored=1")
	if link == nil || link.Path != "/artist/status/123/video/1" {
		t.Fatal(link)
	}
	if err := processLink(bot, nil, db, message, link, nil); err != nil {
		t.Fatal(err)
	}
	direct, err := findArtwork(db, "x:123:photo:1")
	if err != nil || direct == nil || len(direct.Types) != 1 || direct.Types[0] != "video" {
		t.Fatalf("direct: %+v %v", direct, err)
	}
	for _, path := range []string{"/video/2", "/photo/1", "/media/9"} {
		_, err := fetchXPost(inlineLink("https://x.com/artist/status/123"+path), false)
		if _, ok := errors.AsType[unavailablePhotoError](err); !ok {
			t.Fatalf("%s: %v", path, err)
		}
	}
	for _, path := range []string{"/video/1", "/media/1", "/media/2"} {
		link := inlineLink("https://x.com/artist/status/123" + path)
		data := artworkActionData("original", user.ID, link)
		restored := artworkActionLink(strings.Split(data, ":"))
		if restored == nil || restored.Path != link.Path || len(data) > 64 {
			t.Fatalf("callback %s: %q %v", path, data, restored)
		}
	}
	callback := &tgbotapi.CallbackQuery{
		ID: "original", From: user, Message: message,
		Data: artworkActionData("original", user.ID, link),
	}
	handleArtworkCallback(bot, nil, db, callback, queue)
	if len(api.documents) != 1 || api.documents[0] != "https://example.com/video" {
		t.Fatal(api.documents)
	}
}
