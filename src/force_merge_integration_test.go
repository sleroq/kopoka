package main

import (
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestInlineForceMerge(t *testing.T) {
	db := testArtworkDB(t)
	var answer []map[string]any
	var merged image.Image
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user/status/42":
			tweet := map[string]any{
				"author": map[string]string{"name": "Artist"},
				"media":  map[string]any{"photos": []map[string]string{{"url": server.URL + "/red"}, {"url": server.URL + "/blue"}}},
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
		case r.URL.Path == "/red" || r.URL.Path == "/blue":
			bounds, c := image.Rect(0, 0, 200, 300), color.RGBA{255, 0, 0, 255}
			if r.URL.Path == "/blue" {
				bounds, c = image.Rect(0, 0, 100, 100), color.RGBA{0, 0, 255, 255}
			}
			img := image.NewRGBA(bounds)
			draw.Draw(img, bounds, image.NewUniform(c), image.Point{}, draw.Src)
			_ = png.Encode(w, img)
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/answerInlineQuery"):
			_ = r.ParseForm()
			_ = json.Unmarshal([]byte(r.FormValue("results")), &answer)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			f, _, err := r.FormFile("photo")
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			merged, _, err = image.Decode(f)
			if err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"photo":[{"file_id":"merged-file"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
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

	query := &tgbotapi.InlineQuery{ID: "query", From: &tgbotapi.User{ID: 1}, Query: "https://x.com/user/status/42"}
	previews := testPreviews(t, db, bot)
	handleInlineQuery(bot, db, query, previews)
	if len(answer) != 7 || answer[0]["id"] != inlineResultID || answer[1]["id"] != inlineMergedResultID {
		t.Fatalf("uncached: %#v", answer)
	}

	normal := artwork{ID: "x:42", URL: query.Query, Author: "Artist", Photos: []string{"normal-1", "normal-2"}}
	if err := saveSelection(db, 1, normal); err != nil {
		t.Fatal(err)
	}
	handleInlineQuery(bot, db, query, previews)
	if len(answer) != 7 || answer[0]["id"] != "x:42" || answer[1]["id"] != inlineMergedResultID {
		t.Fatalf("cached: %#v", answer)
	}

	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	chosen := &tgbotapi.ChosenInlineResult{
		From: query.From, ResultID: inlineMergedResultID, InlineMessageID: "inline", Query: query.Query,
	}
	handleChosenInlineResult(bot, nil, db, chosen, "@cache", queue, nil)
	if merged == nil || merged.Bounds().Size() != image.Pt(300, 300) {
		t.Fatalf("merged image: %v", merged)
	}

	checkForcedMergePixels(t, merged)

	saved, err := findArtwork(db, "x:42:merged")
	if err != nil || saved == nil || len(saved.Photos) != 1 || saved.Photos[0] != "merged-file" || !strings.Contains(saved.Caption, "Artist") {
		t.Fatalf("merged saved: %#v, %v", saved, err)
	}
	checkBarsSelection(t, bot, db, chosen, queue, previews, &merged)
	query.Query = ""
	answer = nil
	handleInlineQuery(bot, db, query, previews)
	checkMergedHistory(t, answer, saved.Caption)
	unchanged, err := findArtwork(db, "x:42")
	if err != nil || unchanged == nil || strings.Join(unchanged.Photos, ",") != "normal-1,normal-2" {
		t.Fatalf("normal saved: %#v, %v", unchanged, err)
	}
}

func checkMergedHistory(t *testing.T, answer []map[string]any, caption string) {
	t.Helper()
	if len(answer) < 2 {
		t.Fatalf("merged history results: %#v", answer)
	}
	for _, result := range answer[:2] {
		if result["photo_file_id"] != "merged-file" || result["caption"] != caption || result["parse_mode"] != "HTML" || result["input_message_content"] != nil {
			t.Fatalf("merged history result: %#v", result)
		}
	}
}

func checkBarsSelection(t *testing.T, bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, queue *mediaQueue, previews *thumbnails, merged *image.Image) {
	t.Helper()
	chosen.ResultID = inlineBarsResultID
	handleChosenInlineResult(bot, nil, db, chosen, "@cache", queue, previews)
	if *merged == nil || (*merged).Bounds().Size() != image.Pt(302, 300) {
		t.Fatalf("bars image: %v", merged)
	}
	for _, p := range []image.Point{{200, 150}, {201, 150}, {0, 0}, {201, 0}, {250, 250}} {
		r, g, b, _ := (*merged).At(p.X, p.Y).RGBA()
		if r > 0xa000 || g > 0xa000 || b > 0xa000 {
			t.Fatalf("expected black at %v: %x %x %x", p, r, g, b)
		}
	}
	bars, err := findArtwork(db, "x:42:merged:bars")
	if err != nil || bars == nil || len(bars.Photos) != 1 || bars.Photos[0] != "merged-file" {
		t.Fatalf("bars saved: %#v, %v", bars, err)
	}
	if photo := previews.savedPhoto(*bars); photo.fileID != "merged-file" {
		t.Fatalf("bars history preview: %#v", photo)
	}
}

func checkForcedMergePixels(t *testing.T, merged image.Image) {
	t.Helper()
	for _, sample := range []struct {
		p    image.Point
		want color.RGBA
	}{{image.Pt(50, 50), color.RGBA{255, 0, 0, 255}}, {image.Pt(250, 50), color.RGBA{0, 0, 255, 255}}, {image.Pt(250, 250), color.RGBA{255, 255, 255, 255}}} {
		got := color.RGBAModel.Convert(merged.At(sample.p.X, sample.p.Y)).(color.RGBA)
		for _, delta := range []int{int(got.R) - int(sample.want.R), int(got.G) - int(sample.want.G), int(got.B) - int(sample.want.B)} {
			if delta < -20 || delta > 20 {
				t.Fatalf("pixel %v = %v", sample.p, got)
			}
		}
	}

}
