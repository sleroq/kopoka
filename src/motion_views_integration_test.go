package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestMotionViewsIntegration(t *testing.T) {
	motionTestTools(t)
	renderer := testPostImageRenderer(t)
	t.Setenv("KOPOKA_RENDERER", renderer)

	video := postMergeTestVideo(t)
	for _, kind := range []string{"gif", "video"} {
		t.Run(kind, func(t *testing.T) {
			db := testArtworkDB(t)
			var server *httptest.Server
			var uploads []string
			var edits []string
			files := map[string][]byte{
				"/blue": motionTestPNG(t, color.RGBA{B: 255, A: 255}),
				"/red":  motionTestPNG(t, color.RGBA{R: 255, A: 255}),
				"/gif":  motionTestGIF(t, 20), "/video": video,
			}
			server = httptest.NewServer(motionViewHandler(t, kind, &server, files, &uploads, &edits))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			original := http.DefaultTransport
			http.DefaultTransport = redirectTransport{target, original}
			defer func() { http.DefaultTransport = original }()
			link, _ := url.Parse("https://x.com/user/status/42")
			items, err := handleXLink(link, &artwork{}, "merge")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("merge: %d items", len(items))
			}
			want := "animation"
			if kind == "video" {
				want = "video"
			}
			base := mediaBase(items[0])
			if base.Type != want {
				t.Fatalf("merge type %s", base.Type)
			}
			frame := motionTestFrame(t, base.Media.(tgbotapi.FileBytes).Bytes, "0.1")
			// Sample each panel at its center after scaling.
			h := frame.Bounds().Dy()
			motionAssertColor(t, frame, h/2, h/2, 2)
			motionAssertColor(t, frame, frame.Bounds().Dx()/2, h/2, 0)
			motionAssertColor(t, frame, frame.Bounds().Dx()-h/2, h/2, 0)
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			link, _ = url.Parse("https://x.com/user/status/42")
			chosen := &tgbotapi.ChosenInlineResult{From: &tgbotapi.User{ID: 1}, InlineMessageID: "inline"}
			for range 2 {
				if err := editInlinePostImage(bot, db, chosen, "@cache", link, "light", nil); err != nil {
					t.Fatal(err)
				}
			}
			if len(uploads) != 1 || uploads[0] != want || strings.Join(edits, ",") != want+":card,"+want+":card" {
				t.Fatalf("uploads %v edits %v", uploads, edits)
			}
			cached, err := findPostImage(db, artworkID(link), "light", defaultTimezone, defaultTranslationSettings())
			if err != nil || cached.Type != want || cached.Media != tgbotapi.FileID("card") {
				t.Fatalf("cached %+v: %v", cached, err)
			}
			previews := testPreviews(t, db, bot)
			result := postImageResult(db, link, "light", defaultTimezone, defaultTranslationSettings(), previews, nil)
			if result["type"] != "photo" || result["id"] != postImageID(link, "light")+":cached" || result["input_message_content"].(map[string]any)["message_text"] != "Loading post image…" {
				t.Fatalf("inline result %v", result)
			}
		})
	}
}

func TestPostImageMediaMigrationIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	link, _ := url.Parse("https://x.com/user/status/42")
	if err := savePostImage(db, link, "light", defaultTimezone, defaultTranslationSettings(), "photo", "old", "caption"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE post_image_cache DROP COLUMN media_type`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cached, err := findPostImage(db, artworkID(link), "light", defaultTimezone, defaultTranslationSettings())
	if err != nil || cached.Type != "photo" || cached.Media != tgbotapi.FileID("old") {
		t.Fatalf("migrated %+v: %v", cached, err)
	}
	for _, kind := range []string{"animation", "video"} {
		if err := savePostImage(db, link, "light", defaultTimezone, defaultTranslationSettings(), kind, "new", "caption"); err != nil {
			t.Fatal(err)
		}
		cached, err = findPostImage(db, artworkID(link), "light", defaultTimezone, defaultTranslationSettings())
		if err != nil || cached.Type != kind || cached.Media != tgbotapi.FileID("new") {
			t.Fatalf("roundtrip %+v: %v", cached, err)
		}
	}
}

func motionViewHandler(t *testing.T, kind string, server **httptest.Server, files map[string][]byte, uploads, edits *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case (r.URL.Path == "/user/status/42" || r.URL.Path == "/user/status/42/en"):
			media := []map[string]string{
				{"type": "photo", "url": (*server).URL + "/blue"},
				{"type": kind, "url": (*server).URL + "/" + kind, "thumbnail_url": (*server).URL + "/red"},
				{"type": "photo", "url": (*server).URL + "/red"},
			}

			tweet := map[string]any{
				"id": "42", "text": "hello",
				"author": map[string]string{"name": "Artist", "screen_name": "user"},
				"media":  map[string]any{"all": media},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
		case files[r.URL.Path] != nil:
			_, _ = w.Write(files[r.URL.Path])
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendAnimation"), strings.HasSuffix(r.URL.Path, "/sendVideo"):
			typ := "animation"
			if strings.HasSuffix(r.URL.Path, "/sendVideo") {
				typ = "video"
			}
			file, _, err := r.FormFile(typ)
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Error(err)
				return
			}
			_ = motionTestFrame(t, data, "0.1")
			*uploads = append(*uploads, typ)
			response := `{"ok":true,"result":{"message_id":1,%q:{"file_id":"card","width":32,"height":32,"duration":1}}}`
			_, _ = fmt.Fprintf(w, response, typ)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
			var media struct {
				Type  string `json:"type"`
				Media string `json:"media"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
				t.Error(err)
			}
			*edits = append(*edits, media.Type+":"+media.Media)
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}
