package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestInlineTelegramFlow(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
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
			query := &tgbotapi.InlineQuery{ID: "query", Query: " https://x.com/user/status/1?tracking=1 "}
			handleInlineQuery(bot, query)
			if strings.Join(calls, ",") != "answer" || len(answer) != 1 {
				t.Fatalf("calls: %v, answer: %#v", calls, answer)
			}
			checkInlineAnswer(t, answer[0])
			chosen := &tgbotapi.ChosenInlineResult{ResultID: inlineResultID, InlineMessageID: "inline", Query: query.Query}
			handleChosenInlineResult(bot, nil, chosen, cacheChat)
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
		})
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

func TestInlineEmptyAnswers(t *testing.T) {
	for _, query := range []string{"", "not a URL", "https://example.com/a", "https://pixiv.net/not-art", "https://pixiv.net/artworks/not-a-number"} {
		t.Run(query, func(t *testing.T) {
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
			handleInlineQuery(bot, &tgbotapi.InlineQuery{ID: "id", Query: query})
		})
	}
}

func TestInlineChosenFailures(t *testing.T) {
	for _, query := range []string{"invalid", "https://x.com/user/status/1"} {
		t.Run(query, func(t *testing.T) {
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
			chosen := &tgbotapi.ChosenInlineResult{ResultID: inlineResultID, InlineMessageID: "inline", Query: query}
			handleChosenInlineResult(bot, nil, chosen, "@cache")
			if edits != 1 {
				t.Fatalf("edits: %d", edits)
			}

			chosen.ResultID = "other"
			handleChosenInlineResult(bot, nil, chosen, "@cache")
			handleChosenInlineResult(bot, nil, &tgbotapi.ChosenInlineResult{ResultID: inlineResultID, Query: query}, "@cache")
			if edits != 1 {
				t.Fatalf("unexpected edits: %d", edits)
			}
		})
	}
}

func TestInlineRetryTelegram(t *testing.T) {
	for _, failUpload := range []bool{false, true} {
		t.Run(fmt.Sprintf("failUpload=%v", failUpload), func(t *testing.T) {
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
				chosen := &tgbotapi.ChosenInlineResult{
					ResultID: inlineResultID, InlineMessageID: "inline", Query: "https://x.com/user/status/1",
				}
				handleChosenInlineResult(bot, nil, chosen, "@cache")
				close(done)
			}()
			if !failUpload {
				<-limited
				answered := make(chan struct{})
				go func() { handleInlineQuery(bot, &tgbotapi.InlineQuery{ID: "other"}); close(answered) }()
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
