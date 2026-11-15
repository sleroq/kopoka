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
		case "/user/status/1":
			count = 1
		case "/user/status/2":
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
}
