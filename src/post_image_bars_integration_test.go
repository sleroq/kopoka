package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPostImageBarsInlineResults(t *testing.T) {
	var asset bytes.Buffer
	if err := jpeg.Encode(&asset, image.NewRGBA(image.Rect(0, 0, 10, 10)), nil); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		count := 0
		switch r.URL.Path {
		case "/user/status/1", "/user/status/1/en":
			count = 1
		case "/user/status/2", "/user/status/2/en":
			count = 2
		}
		photos := make([]postImage, count)
		for i := range photos {
			photos[i].URL = server.URL + "/asset"
		}
		if r.URL.Path == "/asset" {
			_, _ = w.Write(asset.Bytes())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"text": "hello", "author": map[string]string{"name": "Artist", "screen_name": "user"},
			"media": map[string]any{"photos": photos},
		}})
	}))
	defer server.Close()

	dir := t.TempDir()
	renderer := filepath.Join(dir, "renderer")
	if err := os.WriteFile(renderer, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", renderer)
	db := testArtworkDB(t)
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	previews := testPreviews(t, db, bot)
	target, _ := url.Parse(server.URL)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}

	for _, tc := range []struct {
		id            string
		instant, bars bool
	}{
		{"3", false, false}, {"1", false, false}, {"2", false, true}, {"3", true, true},
	} {
		user := int64(1)
		if tc.instant {
			user = 2
			if _, err := db.Exec(`INSERT INTO instant_inline_settings(user_id,x) VALUES (2,1)`); err != nil {
				t.Fatal(err)
			}
		}
		link := inlineLink("https://x.com/user/status/" + tc.id + "/photo/2")
		results := inlineArtworkResults(db, user, link, previews, false)
		found := 0
		for _, item := range results {
			result := item.(map[string]any)
			if strings.Contains(result["id"].(string), "-bars:") {
				found++
			}
		}
		if (found == 2) != tc.bars {
			t.Fatalf("id=%s instant=%v bars=%d results=%v", tc.id, tc.instant, found, results)
		}
	}

	for _, tc := range []struct {
		id      string
		user    int64
		artwork int
	}{
		{"3", 1, 0}, {"1", 1, 1}, {"2", 1, 3}, {"3", 2, 3},
	} {
		link := inlineLink("https://x.com/user/status/" + tc.id)
		results := inlineArtworkResults(db, tc.user, link, previews, false)
		if len(results) == 0 || results[len(results)-1].(map[string]any)["id"] != "post:text" {
			t.Fatalf("id=%s user=%d results=%v", tc.id, tc.user, results)
		}
		artwork, postImages := 0, 0
		for _, item := range results {
			id := item.(map[string]any)["id"].(string)
			if id == "post:text" {
				continue
			}
			if strings.HasPrefix(id, "post-image:") {
				postImages++
			} else {
				artwork++
			}
		}
		if artwork != tc.artwork || postImages < 2 {
			t.Fatalf("id=%s user=%d artwork=%d postImages=%d results=%v", tc.id, tc.user, artwork, postImages, results)
		}
	}
}

func TestPostImageBarsCarouselThemes(t *testing.T) {
	dir := t.TempDir()

	var asset bytes.Buffer
	if err := jpeg.Encode(&asset, image.NewRGBA(image.Rect(0, 0, 40, 40)), nil); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "image.jpg")
	if err := os.WriteFile(imagePath, asset.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	renderer := filepath.Join(dir, "renderer")
	script := `#!/bin/sh
input=$(cat)
case "$input" in *'"bars":true'*) touch "$TEST_DIR/bars";; esac
cat "$TEST_IMAGE"
`
	if err := os.WriteFile(renderer, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", renderer)
	t.Setenv("TEST_DIR", dir)
	t.Setenv("TEST_IMAGE", imagePath)

	var fetches atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.URL.Path == "/asset" {
			_, _ = w.Write(asset.Bytes())
			return
		}
		photos := make([]postImage, 5)
		for i := range photos {
			photos[i].URL = server.URL + "/asset"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{
			"author": map[string]string{"name": "Artist", "screen_name": "user"},
			"media":  map[string]any{"photos": photos},
		}})
	}))
	defer server.Close()
	db := testArtworkDB(t)
	previews := testPreviews(t, db, nil)
	target, _ := url.Parse(server.URL)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/user/status/123")

	ids := func(results []any) string {
		var names []string
		for _, item := range results {
			names = append(names, strings.Split(item.(map[string]any)["id"].(string), ":")[1])
		}
		return strings.Join(names, ",")
	}

	if got := ids(inlinePostImageResults(db, 1, link, previews, true)); got != "light,dark,light-bars,dark-bars" {
		t.Fatalf("instant without preparation: %s", got)
	}
	if fetches.Load() != 0 {
		t.Fatal("instant mode fetched post data")
	}
	if got := ids(inlinePostImageResults(db, 1, link, previews, false)); got != "light,dark" {
		t.Fatalf("carousel: %s", got)
	}
	if got := ids(inlinePostImageResults(db, 1, link, previews, true)); got != "light,dark" {
		t.Fatalf("instant with preparation: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "bars")); !os.IsNotExist(err) {
		t.Fatalf("carousel bars rendered: %v", err)
	}

	if err := savePostImageTheme(db, 1, "warm"); err != nil {
		t.Fatal(err)
	}
	if err := savePostImageTheme(db, 1, "dim"); err != nil {
		t.Fatal(err)
	}
	if got := ids(inlinePostImageResults(db, 1, link, previews, false)); got != "warm,dim" {
		t.Fatalf("warm and dim carousel: %s", got)
	}
	if got := ids(inlinePostImageResults(db, 1, link, previews, true)); got != "warm,dim" {
		t.Fatalf("instant warm and dim carousel: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "bars")); !os.IsNotExist(err) {
		t.Fatalf("carousel bars rendered: %v", err)
	}
}
