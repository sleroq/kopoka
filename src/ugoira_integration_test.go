package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func ugoiraTestArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for i, c := range []color.RGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}} {
		frame, err := zw.Create(fmt.Sprintf("%06d.png", i))
		if err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for y := range 32 {
			for x := range 32 {
				img.SetRGBA(x, y, c)
			}
		}
		if err := png.Encode(frame, img); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestPixivUgoiraIntegration(t *testing.T) {
	archive := ugoiraTestArchive(t)
	var answer []map[string]any
	var edit map[string]any
	var uploads []string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			_, _ = fmt.Fprint(w, `{"response":{"access_token":"access","refresh_token":"refresh","expires_in":3600,"user":{}}}`)
		case "/v1/illust/detail":
			_, _ = fmt.Fprint(w, `{"illust":{"id":152026329,"type":"ugoira","title":"animation","user":{"name":"artist"}}}`)
		case "/v1/ugoira/metadata":
			_, _ = fmt.Fprint(w, `{"ugoira_metadata":{"zip_urls":{"medium":"https://i.pximg.net/animation.zip"},
            "frames":[{"file":"000000.png","delay":120},{"file":"000001.png","delay":230},
            {"file":"000002.png","delay":370}]}}`)
		case "/animation.zip":
			if r.Referer() != "https://www.pixiv.net/" {
				t.Error("missing Pixiv referer")
			}
			_, _ = w.Write(archive)
		case "/bottoken/getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		case "/bottoken/answerInlineQuery":
			_ = r.ParseForm()
			if err := json.Unmarshal([]byte(r.FormValue("results")), &answer); err != nil {
				t.Error(err)
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/bottoken/sendChatAction":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/bottoken/sendAnimation":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Error(err)
			}
			uploads = append(uploads, r.FormValue("chat_id"))
			if r.FormValue("chat_id") != "@cache" && (r.FormValue("reply_to_message_id") != "42" || !strings.Contains(r.FormValue("caption"), "artist") || r.FormValue("parse_mode") != "HTML") {
				t.Errorf("chat animation parameters: %v", r.Form)
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"animation":{"file_id":"animation-id"}}}`)
		case "/bottoken/deleteMessage":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		case "/bottoken/editMessageMedia":
			_ = r.ParseForm()
			if err := json.Unmarshal([]byte(r.FormValue("media")), &edit); err != nil {
				t.Error(err)
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base := server.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.ServerName = "127.0.0.1"
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient = &http.Client{}
	http.DefaultTransport = base
	t.Cleanup(func() {
		http.DefaultClient.Transport.(*pixivTransport).pixiv.CloseIdleConnections()
		base.CloseIdleConnections()
		http.DefaultClient, http.DefaultTransport = oldClient, oldTransport
	})
	t.Setenv("PIXIV_ACCESS_TOKEN", "access")
	t.Setenv("PIXIV_REFRESH_TOKEN", "refresh")
	t.Setenv("PIXIV_TOKEN_DEADLINE", "3600")
	app, err := setupPixivClient()
	if err != nil {
		t.Fatal(err)
	}
	link := inlineLink("https://www.pixiv.net/en/artworks/152026329")
	if link == nil || link.Path != "/artworks/152026329" {
		t.Fatalf("localized link not normalized: %v", link)
	}
	art := &artwork{}
	media, err := handlePixivLink(app, link, art)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 1 {
		t.Fatalf("got %d media", len(media))
	}
	animation, ok := media[0].(tgbotapi.InputMediaAnimation)
	if !ok {
		t.Fatalf("got %T, want animation", media[0])
	}
	file, ok := animation.Media.(tgbotapi.FileBytes)
	if !ok || animation.ParseMode != "HTML" || !strings.Contains(animation.Caption, "artist") {
		t.Fatal("missing animation upload or caption")
	}
	checkUgoiraVideo(t, file.Bytes)
	checkPixivInlineFlow(t, server, app, &answer, &edit, &uploads)
}

func checkPixivInlineFlow(t *testing.T, server *httptest.Server, app *pixiv.AppPixivAPI, answer *[]map[string]any, edit *map[string]any, uploads *[]string) {
	t.Helper()

	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	db := testArtworkDB(t)
	message := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: 2}}
	link := inlineLink("https://www.pixiv.net/artworks/152026329")
	if err := processLink(bot, app, db, message, link); err != nil {
		t.Fatal(err)
	}
	chatArt, err := findArtwork(db, artworkID(link))
	if err != nil || chatArt == nil || len(chatArt.Types) != 1 || chatArt.Types[0] != "animation" || chatArt.Photos[0] != "animation-id" {
		t.Fatalf("chat artwork: %+v, %v", chatArt, err)
	}
	// Use a fresh database to exercise the uncached inline path.
	db = testArtworkDB(t)
	query := &tgbotapi.InlineQuery{
		ID: "query", From: &tgbotapi.User{ID: 1},
		Query: "https://www.pixiv.net/en/artworks/152026329",
	}
	handleInlineQuery(bot, db, query, testPreviews(t, db, bot))
	if len(*answer) != 1 || (*answer)[0]["id"] != inlineResultID {
		t.Fatalf("inline answer: %v", *answer)
	}
	chosen := &tgbotapi.ChosenInlineResult{
		From: query.From, ResultID: inlineResultID,
		InlineMessageID: "inline", Query: query.Query,
	}
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	handleChosenInlineResult(bot, app, db, chosen, "@cache", queue)
	if (*edit)["type"] != "animation" || (*edit)["media"] != "animation-id" {
		t.Fatalf("inline edit: %v", *edit)
	}
	handleInlineQuery(bot, db, query, testPreviews(t, db, bot))
	if len(*answer) != 1 || (*answer)[0]["type"] != "photo" || !strings.HasPrefix((*answer)[0]["photo_url"].(string), "https://preview.example.com/thumbnail?") {
		t.Fatalf("cached inline answer: %v", *answer)
	}
	rich := (*answer)[0]["input_message_content"].(map[string]any)["rich_message"].(map[string]any)
	block := rich["blocks"].([]any)[0].(map[string]any)
	media := block["animation"].(map[string]any)
	caption := block["caption"].(map[string]any)["text"].([]any)[0].(map[string]any)
	if block["type"] != "animation" || media["type"] != "animation" || media["media"] != "animation-id" || caption["text"] != "artist" || caption["url"] != link.String() {
		t.Fatalf("cached animation: %#v", block)
	}
	if len(*uploads) != 2 || (*uploads)[0] != "123" || (*uploads)[1] != "@cache" {
		t.Fatalf("animation uploads: %v", *uploads)
	}
}

func checkUgoiraVideo(t *testing.T, video []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, video, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,duration:frame=best_effort_timestamp_time",
		"-of", "json", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v: %s", err, output)
	}
	var probe struct {
		Streams []struct {
			Codec    string `json:"codec_name"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Frames []struct {
			Time string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].Codec != "h264" || len(probe.Frames) != 4 {
		t.Fatalf("unexpected video: %s", output)
	}
	for i, want := range []float64{0, .12, .35, .72} {
		got, err := strconv.ParseFloat(probe.Frames[i].Time, 64)
		if err != nil || got < want-.002 || got > want+.002 {
			t.Fatalf("frame %d timestamp = %q", i, probe.Frames[i].Time)
		}
	}
	duration, err := strconv.ParseFloat(probe.Streams[0].Duration, 64)
	if err != nil || duration < .718 || duration > .722 {
		t.Fatalf("duration = %q; probe: %s", probe.Streams[0].Duration, output)
	}
}
