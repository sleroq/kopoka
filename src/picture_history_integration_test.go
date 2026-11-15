package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type pictureHistoryScenario struct {
	t                        *testing.T
	db                       *sql.DB
	bot                      *tgbotapi.BotAPI
	answer                   []map[string]any
	markup, callback, offset string
}

func newPictureHistoryScenario(t *testing.T, path string) *pictureHistoryScenario {
	t.Helper()
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &pictureHistoryScenario{t: t, db: db}
	t.Cleanup(func() { _ = s.db.Close() })
	server := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(server.Close)
	s.bot, err = tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *pictureHistoryScenario) serveHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"bot"}}`))
		return
	case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
		s.answer = nil
		if err := json.Unmarshal([]byte(r.Form.Get("results")), &s.answer); err != nil {
			s.t.Error(err)
		}
		s.offset = r.Form.Get("next_offset")
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		s.markup = r.Form.Get("reply_markup")
	case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
		s.callback = r.Form.Get("text")
	default:
		s.t.Errorf("unexpected request: %s", r.URL.Path)
	}
	_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
}

func (s *pictureHistoryScenario) save(a artwork, inline bool) {
	s.t.Helper()
	if err := saveArtwork(s.db, 1, a, inline); err != nil {
		s.t.Fatal(err)
	}
}

func (s *pictureHistoryScenario) query(text, cursor string) {
	s.t.Helper()
	q := &tgbotapi.InlineQuery{ID: "q", From: &tgbotapi.User{ID: 1}, Query: text, Offset: cursor}
	handleInlineQuery(s.bot, s.db, q, testPreviews(s.t, s.db, s.bot))
}

func (s *pictureHistoryScenario) toggle(user int64) {
	s.t.Helper()
	s.callback = ""
	q := &tgbotapi.CallbackQuery{ID: "cb", From: &tgbotapi.User{ID: user}, Data: "settings:view:1",
		Message: &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 1}}}
	handleSettingsCallback(s.bot, s.db, q)
}

func TestHistorySourceFilter(t *testing.T) {
	s := newPictureHistoryScenario(t, filepath.Join(t.TempDir(), "history.db"))
	for i := range 52 {
		for _, source := range []string{"x", "pixiv"} {
			id := fmt.Sprintf("%s:%d", source, i)
			a := artwork{ID: id, URL: "https://example.com/" + id, Author: "Artist", Photos: []string{id}}
			s.save(a, true)
		}
	}
	for _, filter := range []struct{ query, source string }{{"x", "x"}, {"p", "pixiv"}} {
		for _, pictures := range []bool{false, true} {
			if pictures {
				s.toggle(1)
			}
			checkHistorySource(t, s, filter.query, filter.source)
			if pictures {
				s.toggle(1)
			}
		}
	}
}

func checkHistorySource(t *testing.T, s *pictureHistoryScenario, query, source string) {
	t.Helper()
	cursor, count := "", 0
	for {
		s.query(query, cursor)
		for _, result := range s.answer {
			if !strings.HasPrefix(result["id"].(string), source+":") {
				t.Fatalf("%s: unexpected result %v", query, result)
			}
			count++
		}
		cursor = s.offset
		if cursor == "" {
			break
		}
	}
	if count != 52 {
		t.Fatalf("%s: got %d results", query, count)
	}
}

func TestPictureHistoryFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := newPictureHistoryScenario(t, path)
	caption := `<a href="https://fxtwitter.com/u/status/1">Artist</a>`
	parent := artwork{ID: "x:1", URL: "https://x.com/u/status/1", Author: "Artist", Caption: caption,
		Photos: []string{"merged", "independent", "video", "animation"}, Types: []string{"photo", "photo", "video", "animation"}}
	s.save(parent, true)
	selected := artwork{ID: "x:2:photo:3", URL: "https://x.com/u/status/2/photo/3", Author: "Artist",
		Caption: `<a href="https://fxtwitter.com/u/status/2/photo/3">Artist</a>`, Photos: []string{"selected"}}
	s.save(selected, true)

	s.query(" ", "")
	if len(s.answer) != 2 || s.answer[0]["id"] != "x:2:photo:3" {
		t.Fatalf("post view: %v", s.answer)
	}
	s.query(" . ", "")
	checkPictureHistoryResults(t, s.answer, caption)

	s.toggle(2)
	if s.callback == "" {
		t.Fatal("unauthorized toggle")
	}
	s.toggle(1)
	if s.callback != "" || !strings.Contains(s.markup, "Default view: Pictures") {
		t.Fatalf("toggle: %s %s", s.callback, s.markup)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s.db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s.query("", "")
	checkPictureHistoryResults(t, s.answer, caption)
	s.query(".", "")
	if len(s.answer) != 2 || s.answer[0]["id"] != "x:2:photo:3" {
		t.Fatalf("inverted posts: %v", s.answer)
	}
	s.toggle(1)
	if !strings.Contains(s.markup, "Default view: Posts") {
		t.Fatal(s.markup)
	}
	checkPictureHistorySelection(t, s, caption)
}

func checkPictureHistoryResults(t *testing.T, answer []map[string]any, caption string) {
	t.Helper()
	items := []struct{ id, kind, key, file string }{
		{"x:2:photo:3:item:0", "photo", "photo_file_id", "selected"},
		{"x:1:item:0", "photo", "photo_file_id", "merged"},
		{"x:1:item:1", "photo", "photo_file_id", "independent"},
		{"x:1:item:2", "video", "video_file_id", "video"},
		{"x:1:item:3", "mpeg4_gif", "mpeg4_file_id", "animation"},
	}

	if len(answer) != len(items) {
		t.Fatalf("picture view: %v", answer)
	}
	for i, want := range items {
		result := answer[i]
		if result["id"] != want.id || result["type"] != want.kind || result[want.key] != want.file {
			t.Fatalf("item %d: %v", i, result)
		}
		if want.kind == "photo" {
			wantCaption := caption
			if i == 0 {
				wantCaption = `<a href="https://fxtwitter.com/u/status/2/photo/3">Artist</a>`
			}
			if result["caption"] != wantCaption || result["parse_mode"] != "HTML" || result["input_message_content"] != nil {
				t.Fatalf("item %d caption: %v", i, result)
			}
			continue
		}
		rich := result["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
		if want.kind == "mpeg4_gif" {
			animation := rich["blocks"].([]any)[0].(map[string]any)
			link := animation["caption"].(map[string]any)["text"].([]any)[0].(map[string]any)
			if link["url"] != "https://x.com/u/status/1" || link["text"] != "Artist" {
				t.Fatalf("animation caption: %v", link)
			}
			continue
		}
		html := rich["html"].(string)
		wantCaption := caption
		if i == 0 {
			wantCaption = `https://fxtwitter.com/u/status/2/photo/3`
		}
		media := rich["media"].([]any)
		if !strings.Contains(html, wantCaption) || strings.Contains(html, "<tg-slideshow>") || len(media) != 1 || media[0].(map[string]any)["media"].(map[string]any)["media"] != want.file {
			t.Fatalf("item %d content: %v", i, rich)
		}
	}
}

func checkPictureHistorySelection(t *testing.T, s *pictureHistoryScenario, caption string) {
	t.Helper()
	// Selecting a chat-origin item updates the existing parent and reveals it.
	chat := artwork{ID: "x:3", URL: "https://x.com/u/status/3", Author: "A", Caption: caption, Photos: []string{"chat"}}
	s.save(chat, false)
	if _, err := s.db.Exec(`INSERT INTO history_chat_hidden(user_id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	s.query(".", "")
	if len(s.answer) != 5 {
		t.Fatalf("hidden: %v", s.answer)
	}

	chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1}, ResultID: "x:3:item:0"}
	handleChosenInlineResult(s.bot, nil, s.db, chosen, "", nil, nil)
	s.query(".", "")
	if len(s.answer) != 6 || s.answer[0]["id"] != "x:3:item:0" {
		t.Fatalf("selection: %v", s.answer)
	}

	chosen.ResultID = "x:2:photo:3:item:0"
	handleChosenInlineResult(s.bot, nil, s.db, chosen, "", nil, nil)
	s.query(".", "")
	if s.answer[0]["id"] != "x:2:photo:3:item:0" {
		t.Fatalf("selected-photo order: %v", s.answer)
	}
	chosen.ResultID = "x:1:item:1"
	handleChosenInlineResult(s.bot, nil, s.db, chosen, "", nil, nil)
	s.query(".", "")
	if s.answer[0]["id"] != "x:1:item:0" {
		t.Fatalf("recent order: %v", s.answer)
	}

	if _, err := s.db.Exec(`INSERT INTO history_first_saved(user_id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	s.query(".", "")
	if s.answer[0]["id"] != "x:3:item:0" {
		t.Fatalf("first-saved order: %v", s.answer)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM history WHERE user_id=1`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history count %d: %v", count, err)
	}
}

func TestPictureHistoryPagination(t *testing.T) {
	singles := make([]int, 51)
	for i := range singles {
		singles[i] = 1
	}

	cases := []struct {
		name    string
		counts  []int
		pages   []int
		offsets []string
	}{
		{"within-post", []int{1, 1, 101}, []int{50, 50, 3}, []string{"0:50", "0:100", ""}},
		{"post-boundary", []int{1, 50}, []int{50, 1}, []string{"1:0", ""}},
		{"exactly-50", []int{50}, []int{50}, []string{""}},
		{"many-posts", singles, []int{50, 1}, []string{"50:0", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkPictureHistoryPages(t, tc.counts, tc.pages, tc.offsets)
		})
	}
}

func checkPictureHistoryPages(t *testing.T, counts, pages []int, offsets []string) {
	t.Helper()
	s := newPictureHistoryScenario(t, filepath.Join(t.TempDir(), "history.db"))
	for p, count := range counts {
		a := artwork{ID: fmt.Sprintf("x:%d", p+1), URL: fmt.Sprintf("https://x.com/u/status/%d", p+1),
			Author: "A", Caption: "caption"}
		for i := range count {
			a.Photos = append(a.Photos, fmt.Sprintf("%d-%d", p, i))
		}
		s.save(a, true)
	}
	var expected []string
	for p, count := range slices.Backward(counts) {
		for i := range count {
			expected = append(expected, fmt.Sprintf("x:%d:item:%d", p+1, i))
		}
	}

	var actual []string
	cursor := ""
	for page, size := range pages {
		s.query(".", cursor)
		if len(s.answer) != size || s.offset != offsets[page] || len(s.offset) > 64 {
			t.Fatalf("page %d: %d results, offset %q", page, len(s.answer), s.offset)
		}
		for _, item := range s.answer {
			id := item["id"].(string)
			if len(id) > 64 {
				t.Fatalf("long result ID: %s", id)
			}
			actual = append(actual, id)
		}
		cursor = s.offset
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("result order: %v, want %v", actual, expected)
	}
}
