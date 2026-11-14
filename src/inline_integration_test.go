package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type redirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.fxtwitter.com" {
		copy := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = r.target.Scheme
		u.Host = r.target.Host
		copy.URL = &u
		return r.base.RoundTrip(copy)
	}
	return r.base.RoundTrip(req)
}

func testArtworkDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openArtworkDB(filepath.Join(t.TempDir(), "art.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestInlineTelegramFlow(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			db := testArtworkDB(t)
			cacheChat := "@cache"
			if count == 2 {
				cacheChat = "-100123"
			}
			var calls []string
			var answer []map[string]any
			var edit url.Values
			var server *httptest.Server

			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/user/status/1":
					calls = append(calls, "fetch")
					photos := make([]map[string]string, count)
					for i := range photos {
						photos[i] = map[string]string{"url": server.URL + "/image"}
					}
					tweet := map[string]any{
						"author": map[string]string{"name": "Artist"},
						"text":   "A real post",
						"media":  map[string]any{"photos": photos},
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
				case r.URL.Path == "/image":
					_, _ = w.Write([]byte("not an image"))
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
				case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
					_ = r.ParseMultipartForm(1024 * 1024)
					_ = r.ParseForm()
					if r.FormValue("chat_id") != cacheChat {
						t.Errorf("chat id = %q", r.FormValue("chat_id"))
					}
					calls = append(calls, "send")
					photo := []map[string]string{{"file_id": "small"}, {"file_id": fmt.Sprintf("large%d", (len(calls)-1)/2)}}
					result := map[string]any{"message_id": len(calls), "photo": photo}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
				case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
					_ = r.ParseForm()
					calls = append(calls, "delete")
					if r.FormValue("chat_id") != cacheChat {
						t.Errorf("delete chat id = %q", r.FormValue("chat_id"))
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				case strings.HasSuffix(r.URL.Path, "/editMessageMedia"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
					_ = r.ParseForm()
					edit = r.Form
					calls = append(calls, strings.TrimPrefix(r.URL.Path, "/bottoken/"))
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
					_ = r.ParseForm()
					calls = append(calls, "answer")
					if r.FormValue("cache_time") != "0" || r.FormValue("inline_query_id") != "query" {
						t.Error("incorrect answer parameters")
					}
					if err := json.Unmarshal([]byte(r.FormValue("results")), &answer); err != nil {
						t.Error(err)
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
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
			query := &tgbotapi.InlineQuery{
				From: &tgbotapi.User{ID: 1}, ID: "query", Query: " https://x.com/user/status/1?tracking=1 ",
			}
			handleInlineQuery(bot, db, query)
			if strings.Join(calls, ",") != "answer" || len(answer) != 1 {
				t.Fatalf("calls: %v, answer: %#v", calls, answer)
			}
			checkInlineAnswer(t, answer[0])
			chosen := &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "inline", Query: query.Query,
			}
			handleChosenInlineResult(bot, nil, db, chosen, cacheChat)
			method := "editMessageMedia"
			if count == 2 {
				method = "editMessageText"
			}
			expected := "answer,fetch," + strings.Repeat("send,delete,", count) + method
			if strings.Join(calls, ",") != expected {
				t.Fatalf("calls: %v", calls)
			}
			if edit.Get("inline_message_id") != "inline" || edit.Get("reply_markup") != `{"inline_keyboard":[]}` {
				t.Fatalf("edit: %v", edit)
			}
			checkInlineEdit(t, count, edit)

			checkCachedInlineFlow(t, bot, db, query, &answer, &calls, edit, count, cacheChat)
		})
	}
}

func checkCachedInlineFlow(t *testing.T, bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.InlineQuery, answer *[]map[string]any, calls *[]string, edit url.Values, count int, cacheChat string) {
	t.Helper()
	// The same status under another host and user resolves to the cached artwork.
	query.Query = "https://vxtwitter.com/another/status/1?ref=2"
	handleInlineQuery(bot, db, query)
	cached := (*answer)[0]
	if cached["type"] != "photo" || cached["id"] != "x:1" || cached["photo_file_id"] != "large1" || cached["title"] != "Artist" || cached["description"] != "A real post" {
		t.Fatalf("cached result: %#v", cached)
	}
	if count == 1 {
		if cached["parse_mode"] != "HTML" || !strings.Contains(cached["caption"].(string), "Artist") {
			t.Fatalf("cached: %#v", cached)
		}
	} else {
		rich := cached["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
		var edited map[string]any
		if err := json.Unmarshal([]byte(edit.Get("rich_message")), &edited); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rich, edited) {
			t.Fatalf("cached slideshow: %#v, edit: %#v", rich, edited)
		}
	}

	before := len(*calls)
	handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 2}, ResultID: cached["id"].(string),
	}, cacheChat)
	if len(*calls) != before {
		t.Fatalf("cached selection made requests: %v", (*calls)[before:])
	}

	query.Query = ""
	query.From.ID = 2
	handleInlineQuery(bot, db, query)
	if len(*answer) != 1 || (*answer)[0]["id"] != "x:1" {
		t.Fatalf("history: %#v", *answer)
	}
	query.From.ID = 3
	handleInlineQuery(bot, db, query)
	if len(*answer) != 0 {
		t.Fatalf("other user's history: %#v", *answer)
	}

	// Reopening the on-disk database retains artwork and histories.
	path := filepath.Join(t.TempDir(), "reopen.db")
	disk, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := findArtwork(db, "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSelection(disk, 1, *a); err != nil {
		t.Fatal(err)
	}
	second := *a
	second.ID = "pixiv:2"
	second.Text = "another"
	if err := saveSelection(disk, 1, second); err != nil {
		t.Fatal(err)
	}
	if err := selectArtwork(disk, 1, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disk.Close() }()
	query.From.ID = 1
	handleInlineQuery(bot, disk, query)
	if len(*answer) != 2 || (*answer)[0]["id"] != "x:1" || (*answer)[1]["id"] != "pixiv:2" {
		t.Fatalf("reopened history: %#v", *answer)
	}
}

func checkInlineAnswer(t *testing.T, result map[string]any) {
	t.Helper()
	content := result["input_message_content"].(map[string]any)
	keyboard := result["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	button := keyboard[0].([]any)[0].(map[string]any)
	if result["type"] != "article" || result["id"] != inlineResultID || content["message_text"] != "Loading artwork…" || content["disable_web_page_preview"] != true || button["text"] != "Please wait…" || button["disabled"] == nil {
		t.Fatalf("answer: %#v", result)
	}
}

func checkInlineEdit(t *testing.T, count int, edit url.Values) {
	t.Helper()
	var payload map[string]any
	field := "media"
	if count == 2 {
		field = "rich_message"
	}
	if err := json.Unmarshal([]byte(edit.Get(field)), &payload); err != nil {
		t.Fatal(err)
	}
	if count == 1 {
		if payload["type"] != "photo" || payload["media"] != "large1" || payload["parse_mode"] != "HTML" || !strings.Contains(payload["caption"].(string), "Artist") {
			t.Fatalf("payload: %#v", payload)
		}
	} else {
		markup := payload["html"].(string)
		media := payload["media"].([]any)
		if !strings.Contains(markup, `<tg-slideshow>`) || !strings.Contains(markup, `tg://photo?id=1`) || !strings.Contains(markup, "Artist") || len(media) != 2 || media[1].(map[string]any)["media"].(map[string]any)["media"] != "large2" {
			t.Fatalf("payload: %#v", payload)
		}
	}
}

func TestInlineHistoryPagination(t *testing.T) {
	db := testArtworkDB(t)
	for i := range 51 {
		a := artwork{ID: fmt.Sprintf("x:%d", i), Author: "Artist", Photos: []string{"photo"}}
		if err := saveSelection(db, 1, a); err != nil {
			t.Fatal(err)
		}
	}

	var results []map[string]any
	var next string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
			return
		}
		_ = r.ParseForm()
		if err := json.Unmarshal([]byte(r.FormValue("results")), &results); err != nil {
			t.Error(err)
		}
		next = r.FormValue("next_offset")
		if r.FormValue("is_personal") != "true" {
			t.Error("history response is not personal")
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	query := &tgbotapi.InlineQuery{ID: "id", From: &tgbotapi.User{ID: 1}}
	handleInlineQuery(bot, db, query)
	if len(results) != 50 || next != "50" {
		t.Fatalf("first page: %d results, next %q", len(results), next)
	}
	query.Offset = next
	handleInlineQuery(bot, db, query)
	if len(results) != 1 || results[0]["id"] != "x:0" || next != "" {
		t.Fatalf("last page: %#v, next %q", results, next)
	}
}

func TestInlineEmptyAnswers(t *testing.T) {
	for _, query := range []string{"", "not a URL", "https://example.com/a", "https://pixiv.net/not-art", "https://pixiv.net/artworks/not-a-number"} {
		t.Run(query, func(t *testing.T) {
			db := testArtworkDB(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/answerInlineQuery") {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				_ = r.ParseForm()
				if r.FormValue("results") != "[]" || r.FormValue("cache_time") != "0" {
					t.Errorf("answer: %v", r.Form)
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			handleInlineQuery(bot, db, &tgbotapi.InlineQuery{From: &tgbotapi.User{ID: 1}, ID: "id", Query: query})
		})
	}
}

func TestInlineChosenFailures(t *testing.T) {
	for _, query := range []string{"invalid", "https://x.com/user/status/1"} {
		t.Run(query, func(t *testing.T) {
			db := testArtworkDB(t)
			var edits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
				case r.URL.Path == "/user/status/1":
					http.Error(w, "unavailable", http.StatusBadGateway)
				case strings.HasSuffix(r.URL.Path, "/editMessageText"):
					_ = r.ParseForm()
					edits++
					if r.FormValue("inline_message_id") != "inline" || !strings.Contains(r.FormValue("text"), "Could not load artwork") || r.FormValue("reply_markup") != `{"inline_keyboard":[]}` {
						t.Errorf("edit: %v", r.Form)
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
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
			chosen := &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "inline", Query: query,
			}
			handleChosenInlineResult(bot, nil, db, chosen, "@cache")
			if edits != 1 {
				t.Fatalf("edits: %d", edits)
			}

			chosen.ResultID = "other"
			handleChosenInlineResult(bot, nil, db, chosen, "@cache")
			handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, Query: query,
			}, "@cache")
			if edits != 1 {
				t.Fatalf("unexpected edits: %d", edits)
			}
		})
	}
}

func TestInlineRetryTelegram(t *testing.T) {
	for _, failUpload := range []bool{false, true} {
		t.Run(fmt.Sprintf("failUpload=%v", failUpload), func(t *testing.T) {
			db := testArtworkDB(t)
			var server *httptest.Server
			var sends, edits, failures int
			limited := make(chan struct{})

			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/user/status/1":
					_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"Artist"},"media":{"photos":[{"url":%q}]}}}`,
						server.URL+"/image")
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
				case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
					sends++
					switch {
					case failUpload:
						_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"bad photo"}`))
					case sends == 1:
						close(limited)
						_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":1}}`))
					default:
						_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"photo"}]}}`))
					}
				case strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
					edits++
					if edits == 1 {
						_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":1}}`))
					} else {
						_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
					}
				case strings.HasSuffix(r.URL.Path, "/editMessageText"):
					failures++
					_ = r.ParseForm()
					if !strings.Contains(r.FormValue("text"), "Could not load artwork") {
						t.Errorf("failure edit: %v", r.Form)
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
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
			done := make(chan struct{})
			go func() {
				chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1},
					ResultID: inlineResultID, InlineMessageID: "inline", Query: "https://x.com/user/status/1",
				}
				handleChosenInlineResult(bot, nil, db, chosen, "@cache")
				close(done)
			}()
			if !failUpload {
				<-limited
				answered := make(chan struct{})
				go func() {
					handleInlineQuery(bot, db, &tgbotapi.InlineQuery{From: &tgbotapi.User{ID: 1}, ID: "other"})
					close(answered)
				}()
				select {
				case <-answered:
				case <-time.After(500 * time.Millisecond):
					t.Error("inline answer blocked by retry")
				}
			}
			<-done
			if failUpload {
				if sends != 1 || failures != 1 || edits != 0 {
					t.Fatalf("sends=%d failures=%d edits=%d", sends, failures, edits)
				}
			} else if sends != 2 || edits != 2 || failures != 0 {
				t.Fatalf("sends=%d edits=%d failures=%d", sends, edits, failures)
			}
		})
	}
}

func TestInlineMultipartRetry(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			sends++
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
			} else {
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || string(data) != "image bytes" {
					t.Errorf("file = %q, %v", data, err)
				}
			}
			if sends <= 2 {
				_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":1}}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"photo"}]}}`))
			}
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "photo.jpg", Bytes: []byte("image bytes")})
	id, err := cacheInlinePhoto(bot, "@cache", photo)
	if err != nil || id != "photo" || sends != 3 {
		t.Fatalf("id=%q sends=%d err=%v", id, sends, err)
	}
}

func TestSendPhotosRetry(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var sends int
			method := "/sendPhoto"
			if count == 2 {
				method = "/sendMediaGroup"
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
				case strings.HasSuffix(r.URL.Path, method):
					sends++
					if err := r.ParseMultipartForm(1024 * 1024); err != nil {
						t.Error(err)
					}
					if count == 1 {
						file, _, err := r.FormFile("photo")
						if err != nil {
							t.Error(err)
						} else {
							data, _ := io.ReadAll(file)
							_ = file.Close()
							if string(data) != "bytes" {
								t.Errorf("photo = %q", data)
							}
						}
					} else if len(r.MultipartForm.File) != 2 {
						t.Errorf("files: %v", r.MultipartForm.File)
					}
					switch {
					case sends == 1:
						_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":1}}`))
					case count == 1:
						_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
					default:
						_, _ = w.Write([]byte(`{"ok":true,"result":[{"message_id":1},{"message_id":2}]}`))
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			var photos []any
			for range count {
				photos = append(photos, tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "photo.jpg", Bytes: []byte("bytes")}))
			}
			if err := sendPhotos(bot, 1, 2, photos); err != nil || sends != 2 {
				t.Fatalf("sends=%d err=%v", sends, err)
			}
		})
	}
}

func historyPreferenceServer(t *testing.T, answers *[]map[string]any, reply *string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/status/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
				"author": map[string]string{"name": "Artist"}, "media": map[string]any{"photos": []map[string]string{{"url": server.URL + "/image"}}},
			}})
		case r.URL.Path == "/image":
			_, _ = w.Write([]byte("image"))
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"kopoka_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_ = r.ParseForm()
			*reply = r.FormValue("text")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43}}`))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43,"photo":[{"file_id":"photo"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			_ = r.ParseForm()
			if err := json.Unmarshal([]byte(r.FormValue("results")), answers); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	return server
}

func TestHistoryPreferenceTelegramFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var answers []map[string]any
	var reply string
	server := historyPreferenceServer(t, &answers, &reply)
	defer server.Close()
	target, _ := url.Parse(server.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	defer func() { http.DefaultTransport = original }()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	command := func(text string) {
		t.Helper()
		message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 1}, Text: text,
			Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Length: len(text)}}}
		handleMessage(bot, nil, db, message)
	}
	selectCached := func(user int64, id string) {
		chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: user}, ResultID: id}
		handleChosenInlineResult(bot, nil, db, chosen, "@cache")
	}
	selectNew := func(id string) {
		chosen := &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID,
			InlineMessageID: "inline", Query: "https://x.com/user/status/" + id,
		}
		handleChosenInlineResult(bot, nil, db, chosen, "@cache")
	}
	checkHistory := func(user int64, ids ...string) {
		t.Helper()
		handleInlineQuery(bot, db, &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: user}})
		if len(answers) != len(ids) {
			t.Fatalf("user %d history: %#v, want %v", user, answers, ids)
		}
		for i, id := range ids {
			if answers[i]["id"] != id {
				t.Fatalf("user %d history: %#v, want %v", user, answers, ids)
			}
		}
	}

	selectNew("1")
	selectCached(2, "x:1")
	checkHistory(1, "x:1")

	command("/historyoff@kopoka_bot")
	if !strings.Contains(reply, "deleted") {
		t.Fatalf("reply: %q", reply)
	}
	checkHistory(1)
	checkHistory(2, "x:1")
	selectCached(1, "x:1")
	selectNew("2")
	checkHistory(1)
	selectCached(2, "x:2")
	checkHistory(2, "x:2", "x:1")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	selectCached(1, "x:2")
	selectNew("3")
	checkHistory(1)
	command("/historyon")
	if !strings.Contains(reply, "future") {
		t.Fatalf("reply: %q", reply)
	}
	checkHistory(1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	selectCached(1, "x:1")
	selectNew("4")
	checkHistory(1, "x:4", "x:1")
}
