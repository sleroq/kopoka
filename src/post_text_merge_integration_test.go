package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type postTextMergeCase struct {
	name       string
	root       []string
	quote      []string
	wantType   string
	wantURL    string
	wantWidth  int
	wantColors []color.RGBA
	positions  []int
}

func TestPostTextInlinePhotoMerge(t *testing.T) {
	cases := []postTextMergeCase{
		{name: "root and quote", root: []string{"red", "blue"}, quote: []string{"green"}, wantType: "photo", wantWidth: 700, wantColors: []color.RGBA{{255, 0, 0, 255}, {0, 0, 255, 255}, {0, 255, 0, 255}}},
		{name: "download failure", root: []string{"red", "broken"}, wantType: "photo", wantURL: "/red"},
		{name: "video first", root: []string{"video", "red", "blue"}, wantType: "video", wantWidth: 700, positions: []int{100, 300, 550}, wantColors: []color.RGBA{{255, 255, 0, 255}, {255, 0, 0, 255}, {0, 0, 255, 255}}},
		{name: "single photo", root: []string{"red"}, wantType: "photo", wantURL: "/red"},
		{name: "video includes quote", root: []string{"red", "blue", "video"}, quote: []string{"green"}, wantType: "video", wantWidth: 900, positions: []int{100, 350, 600, 800}, wantColors: []color.RGBA{{255, 0, 0, 255}, {0, 0, 255, 255}, {255, 255, 0, 255}, {0, 255, 0, 255}}},
		{name: "single photo before video", root: []string{"red", "video"}, quote: []string{"blue"}, wantType: "video", wantWidth: 700, positions: []int{100, 300, 550}, wantColors: []color.RGBA{{255, 0, 0, 255}, {255, 255, 0, 255}, {0, 0, 255, 255}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			motionTestTools(t)
			video := postMergeTestVideo(t)
			db := testArtworkDB(t)
			var server *httptest.Server
			var uploads []string
			var downloads []string
			var merged image.Image
			var edit url.Values
			var editMethod string
			handler := postTextMergeHandler(t, tc, &server, &uploads, &downloads, &merged, &edit, &editMethod, video)
			server = httptest.NewServer(handler)
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = redirectTransport{target, original}
			defer func() { http.DefaultTransport = original }()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
			handleChosenInlineResult(bot, nil, db, &tgbotapi.ChosenInlineResult{
				From: &tgbotapi.User{ID: 1}, ResultID: inlinePostTextID, InlineMessageID: "inline", Query: "https://x.com/user/status/42",
			}, "@cache", queue, nil)

			wantUpload := tc.wantType + ":multipart"
			if tc.wantURL != "" {
				wantUpload = tc.wantType + ":" + server.URL + tc.wantURL
			}
			if len(uploads) != 1 || uploads[0] != wantUpload {
				t.Errorf("uploads = %v, want [%s]", uploads, wantUpload)
			}
			if tc.wantWidth > 0 {
				if merged == nil || merged.Bounds().Size() != image.Pt(tc.wantWidth, 300) {
					t.Errorf("merged image = %v, want %dx300", merged, tc.wantWidth)
				} else {
					positions := tc.positions
					if positions == nil {
						positions = []int{100, 350, 600}
					}
					for i, x := range positions[:len(tc.wantColors)] {
						got := color.RGBAModel.Convert(merged.At(x, 150)).(color.RGBA)
						want := tc.wantColors[i]
						if absColorDifference(got, want) > 30 {
							t.Errorf("pixel at %d = %v, want %v", x, got, want)
						}
					}
				}
			}
			var media struct {
				Type      string `json:"type"`
				Media     string `json:"media"`
				Caption   string `json:"caption"`
				ParseMode string `json:"parse_mode"`
				Above     bool   `json:"show_caption_above_media"`
			}
			if err := json.Unmarshal([]byte(edit.Get("media")), &media); err != nil {
				t.Fatalf("edit %s %v: %v", editMethod, edit, err)
			}
			if !strings.HasSuffix(editMethod, "/editMessageMedia") || edit.Get("inline_message_id") != "inline" || media.Type != tc.wantType || media.Media != "cached-file" || media.ParseMode != "HTML" || !media.Above || !strings.Contains(media.Caption, "Main &lt;text&gt; &amp; more") || !strings.Contains(media.Caption, "Quoted text") || strings.Contains(media.Caption, "Attachment ") {
				t.Errorf("edit %s: %v, media %+v", editMethod, edit, media)
			}
		})
	}
}

func absColorDifference(a, b color.RGBA) int {
	diff := func(x, y uint8) int {
		d := int(x) - int(y)
		if d < 0 {
			return -d
		}
		return d
	}
	return diff(a.R, b.R) + diff(a.G, b.G) + diff(a.B, b.B)
}

func postTextMergeHandler(t *testing.T, tc postTextMergeCase, server **httptest.Server, uploads, downloads *[]string, merged *image.Image, edit *url.Values, editMethod *string, video []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		switch {
		case r.URL.Path == "/user/status/42":
			attachments := func(names []string) []map[string]string {
				items := make([]map[string]string, 0, len(names))
				for _, name := range names {
					kind := "photo"
					if name == "video" {
						kind = "video"
					}
					items = append(items, map[string]string{"type": kind, "url": (*server).URL + "/" + name})
				}
				return items
			}
			tweet := map[string]any{
				"id": "42", "url": "https://x.com/user/status/42", "text": "Main <text> & more",
				"author": map[string]string{"name": "Root", "screen_name": "user"},
				"media":  map[string]any{"all": attachments(tc.root)},
				"quote": map[string]any{"id": "43", "url": "https://x.com/quote/status/43", "text": "Quoted text",
					"author": map[string]string{"name": "Quote", "screen_name": "quote"}, "media": map[string]any{"all": attachments(tc.quote)}},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
		case r.URL.Path == "/red" || r.URL.Path == "/blue" || r.URL.Path == "/green":
			*downloads = append(*downloads, r.URL.Path)
			bounds, c := image.Rect(0, 0, 200, 300), color.RGBA{255, 0, 0, 255}
			switch r.URL.Path {
			case "/blue":
				bounds, c = image.Rect(0, 0, 100, 100), color.RGBA{0, 0, 255, 255}
			case "/green":
				bounds, c = image.Rect(0, 0, 100, 150), color.RGBA{0, 255, 0, 255}
			}
			img := image.NewRGBA(bounds)
			draw.Draw(img, bounds, image.NewUniform(c), image.Point{}, draw.Src)
			_ = png.Encode(w, img)
		case r.URL.Path == "/video":
			*downloads = append(*downloads, r.URL.Path)
			_, _ = w.Write(video)
		case r.URL.Path == "/broken":
			*downloads = append(*downloads, r.URL.Path)
			http.Error(w, "unavailable", http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"), strings.HasSuffix(r.URL.Path, "/sendVideo"):
			kind := "photo"
			if strings.HasSuffix(r.URL.Path, "/sendVideo") {
				kind = "video"
			}
			if r.FormValue("chat_id") != "@cache" {
				t.Errorf("cache chat: %q", r.FormValue("chat_id"))
			}
			file, _, err := r.FormFile(kind)
			if err == nil {
				defer func() { _ = file.Close() }()
				if kind == "video" {
					data, readErr := io.ReadAll(file)
					if readErr != nil {
						t.Fatal(readErr)
					}
					*merged = motionTestFrame(t, data, "0.1")
				} else {
					*merged, _, err = image.Decode(file)
				}
				if err != nil {
					t.Errorf("decode upload: %v", err)
				}
				*uploads = append(*uploads, kind+":multipart")
			} else {
				*uploads = append(*uploads, kind+":"+r.FormValue(kind))
			}
			if kind == "photo" {
				_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"cached-file"}]}}`)
			} else {
				_, _ = fmt.Fprint(w, `{ "ok":true,"result":{ "message_id":1,
 "video":{"file_id":"cached-file","width":1,"height":1,"duration":1}}}`)
			}
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageMedia"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
			_ = r.ParseForm()
			*edit, *editMethod = r.Form, r.URL.Path
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func postMergeTestVideo(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=yellow:s=200x300:r=10",
		"-t", "0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
