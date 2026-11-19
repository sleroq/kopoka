package main

import (
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestAutomaticPostImages(t *testing.T) {
	renderer := testPostImageRenderer(t)
	t.Setenv("KOPOKA_RENDERER", renderer)
	db := testArtworkDB(t)

	var uploads, reused, edits, fetches int
	server := automaticPostImageServer(t, &uploads, &reused, &edits, &fetches)
	defer server.Close()

	target, _ := url.Parse(server.URL)
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
	chat := func(link string) error { return processLink(bot, nil, db, message, inlineLink(link), nil) }
	if err := chat("https://x.com/user/status/1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", filepath.Join(t.TempDir(), "missing"))
	if err := chat("https://x.com/user/status/1"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KOPOKA_RENDERER", renderer)
	chosen := &tgbotapi.ChosenInlineResult{
		From: message.From, ResultID: inlineResultID, InlineMessageID: "inline", Query: "https://x.com/user/status/2",
	}
	if err := editInlineArtwork(bot, nil, db, chosen, "@cache", nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", filepath.Join(t.TempDir(), "missing"))
	chosen.ResultID = inlineMergedResultID
	if err := editInlineArtwork(bot, nil, db, chosen, "@cache", nil, nil); err != nil {
		t.Fatal(err)
	}

	before := fetches
	if err := chat("https://x.com/user/status/3"); err == nil {
		t.Fatal("fetch failure became card")
	}
	if fetches != before+1 {
		t.Fatalf("fetch failure requests: %d", fetches-before)
	}
	if err := chat("https://x.com/user/status/2/photo/2"); err == nil {
		t.Fatal("unavailable photo became card")
	}
	if uploads != 2 || reused != 1 || edits != 2 {
		t.Fatalf("uploads %d reused %d edits %d", uploads, reused, edits)
	}

	var artworks, cache, history int
	if err := db.QueryRow(`SELECT count(*) FROM artwork`).Scan(&artworks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM post_image_cache WHERE theme='dark' AND file_id='card'`).Scan(&cache); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM history`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if artworks != 0 || cache != 2 || history != 0 {
		t.Fatalf("artwork %d cache %d history %d", artworks, cache, history)
	}
}

func automaticPostImageServer(t *testing.T, uploads, reused, edits, fetches *int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/status/"):
			(*fetches)++
			if strings.HasSuffix(r.URL.Path, "/3") {
				_, _ = fmt.Fprint(w, `{"code":404,"message":"missing"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"Hello world","author":{"name":"Artist","screen_name":"user"}}}`)

		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)

		case strings.HasSuffix(r.URL.Path, "/sendChatAction"), strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)

		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			_ = r.ParseMultipartForm(10 << 20)
			if r.FormValue("chat_id") == "123" && r.FormValue("reply_to_message_id") != "42" {
				t.Errorf("reply: %v", r.Form)
			}
			if r.FormValue("chat_id") == "123" && (!strings.Contains(r.FormValue("caption"), "fxtwitter.com/user/status/1") || r.FormValue("parse_mode") != "HTML") {
				t.Errorf("caption: %v", r.Form)
			}
			if r.FormValue("photo") == "card" {
				(*reused)++
			} else {
				f, _, err := r.FormFile("photo")
				if err != nil {
					t.Error(err)
				} else {
					picture, err := jpeg.Decode(f)
					if err != nil {
						t.Error(err)
					} else if red, _, _, _ := picture.At(12, 12).RGBA(); red > 20000 {
						t.Errorf("not dark: %d", red)
					}
					_ = f.Close()
				}
				(*uploads)++
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":43,"photo":[{"file_id":"card"}]}}`)

		case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
			_ = r.ParseForm()
			var media struct {
				Media     string `json:"media"`
				Caption   string `json:"caption"`
				ParseMode string `json:"parse_mode"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
				t.Error(err)
			}
			if r.FormValue("inline_message_id") != "inline" || media.Media != "card" || !strings.Contains(media.Caption, "fxtwitter.com/user/status/2") || media.ParseMode != "HTML" {
				t.Errorf("edit: %v %+v", r.Form, media)
			}
			(*edits)++
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)

		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
}
