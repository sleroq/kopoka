package main

import (
	"bytes"
	"database/sql"
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
	"sync"
	"testing"
)

func TestPostImageTranslationIntegration(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		author := map[string]string{"name": "Artist", "screen_name": "artist"}
		tweet := map[string]any{
			"author": author, "text": "original",
			"translation": map[string]string{"source_lang": "ru", "target_lang": "en", "text": "translated"},
		}
		if strings.Contains(r.URL.Path, "/42/") {
			if strings.HasSuffix(r.URL.Path, "/fr") {
				tweet["translation"] = map[string]string{"source_lang": "ja", "target_lang": "fr", "text": ""}
			}
			tweet["replying_to_status"] = "41"
			tweet["quote"] = map[string]any{
				"author": author, "text": "quote original",
				"translation": map[string]string{"source_lang": "ja", "target_lang": "en", "text": "quote translated"},
			}
		} else {
			source := "ru"
			if strings.HasSuffix(r.URL.Path, "/fr") {
				source = "fr"
			}
			tweet["translation"] = map[string]string{"source_lang": source, "target_lang": "en", "text": "parent translated"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": tweet})
	}))
	defer server.Close()

	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link := inlineLink("https://x.com/artist/status/42/photo/2")
	defaults := defaultTranslationSettings()
	req, err := preparePostImageWithClient(client, link, "light", "UTC", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if req.Post.Text != "original" || req.Post.TranslationSource != "" || req.Post.Quote.Text != "quote translated" || req.Post.Quote.TranslationSource != "Japanese" || req.ReplyChain[0].Text != "original" || req.ReplyChain[0].TranslationSource != "" {
		t.Fatalf("request: %+v quote=%+v chain=%+v", req.Post, req.Post.Quote, req.ReplyChain)
	}
	if strings.Join(paths, ",") != "/artist/status/42/en,/i/status/41/en" {
		t.Fatalf("paths: %v", paths)
	}

	all := translationSettings{Target: "en"}
	req, err = preparePostImageWithClient(client, link, "light", "UTC", all)
	if err != nil || req.Post.Text != "translated" || req.Post.TranslationSource != "Russian" || req.ReplyChain[0].Text != "parent translated" || req.ReplyChain[0].TranslationSource != "Russian" {
		t.Fatalf("translated: %+v %v", req.Post, err)
	}

	fallback, err := preparePostImageWithClient(client, link, "light", "UTC", translationSettings{Target: "fr"})
	if err != nil || fallback.Post.Text != "original" || fallback.Post.TranslationSource != "" || fallback.ReplyChain[0].Text != "original" || fallback.ReplyChain[0].TranslationSource != "" {
		t.Fatalf("fallback: %+v %+v %v", fallback.Post, fallback.ReplyChain, err)
	}
	if strings.Contains(postImageCaption(link, req), "/en") {
		t.Fatal("caption contains translation suffix")
	}
}

func TestPostImageTranslationCacheIntegration(t *testing.T) {
	link := inlineLink("https://x.com/artist/status/42/photo/2")
	defaults := defaultTranslationSettings()
	all := translationSettings{Target: "en"}
	db := testArtworkDB(t)
	if err := savePostImage(db, link, "light", "UTC", defaults, "photo", "default", "caption"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		policy translationSettings
		want   string
	}{{defaults, "default"}, {all, ""}, {translationSettings{Target: "fr", Ignored: []string{"ru"}}, ""}} {
		media, err := findPostImage(db, artworkID(xPostLink(link)), "light", "UTC", tc.policy)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" {
			if media.Type != "" {
				t.Fatalf("unexpected cache hit for %s: %+v", tc.policy.key(), media)
			}
		} else if fmt.Sprint(media.Media) != tc.want {
			t.Fatalf("cache %s: %+v", tc.policy.key(), media)
		}
	}
}

func TestPostImageTranslationPreviewIntegration(t *testing.T) {
	link := inlineLink("https://x.com/artist/status/42/photo/2")
	defaults := defaultTranslationSettings()
	all := translationSettings{Target: "en"}
	db := testArtworkDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"code":200,"tweet":{"text":"original","author":{"name":"Artist","screen_name":"artist"},"translation":{"source_lang":"ru","target_lang":"en","text":"translated"}}}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}

	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewRGBA(image.Rect(0, 0, 20, 20)), nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.jpg")
	renderer := filepath.Join(dir, "renderer")
	if err := os.WriteFile(imagePath, img.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renderer, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+imagePath+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOPOKA_RENDERER", renderer)
	previews := testPreviews(t, db, nil)
	previews.client = client
	first := previews.startPostImagePreview(link, "UTC", defaults, "light")
	if first != previews.startPostImagePreview(link, "UTC", defaults, "light") {
		t.Fatal("identical policy did not share work")
	}
	second := previews.startPostImagePreview(link, "UTC", all, "light")
	french := translationSettings{Target: "fr", Ignored: []string{"ru"}}
	third := previews.startPostImagePreview(link, "UTC", french, "light")
	defer func() {
		for _, work := range []*postImagePreviewWork{first, second, third} {
			for _, variant := range work.variants {
				<-variant.done
			}
		}
	}()
	if first == second || first == third || second == third {
		t.Fatal("different policies shared work")
	}
	photo := previews.postImagePreview(link, "light", "UTC", all, second)
	u, err := url.Parse(photo.photoURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("target") != "en" || !u.Query().Has("ignored") || u.Query().Get("ignored") != "" {
		t.Fatalf("preview URL %s", u)
	}
	defaultPhoto := previews.postImagePreview(link, "light", "UTC", defaults, first)
	frenchPhoto := previews.postImagePreview(link, "light", "UTC", french, third)
	if photo.photoURL == defaultPhoto.photoURL || photo.photoURL == frenchPhoto.photoURL {
		t.Fatal("different policies shared URL")
	}
	fresh := testPreviews(t, db, nil)
	fresh.client = client
	rec := httptest.NewRecorder()
	fresh.servePostImagePreview(rec, httptest.NewRequest("GET", u.String(), nil))
	served := fresh.existingPostImagePreview(link, "light", "UTC", all)
	if served == nil || served.request.Post.Text != "translated" {
		t.Fatal("served preview did not use URL policy")
	}
	fresh.mu.Lock()
	servedWork := fresh.postImageWork[artworkID(xPostLink(link))+":UTC:"+all.key()]
	fresh.mu.Unlock()
	for _, variant := range servedWork.variants {
		<-variant.done
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("serve: %d %s", rec.Code, rec.Body.String())
	}
	q := u.Query()
	q.Set("ignored", "bogus")
	u.RawQuery = q.Encode()
	rec = httptest.NewRecorder()
	previews.servePostImagePreview(rec, httptest.NewRequest("GET", u.String(), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy: %d", rec.Code)
	}
}

func TestPostImageTranslationMigrationIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE post_image_cache (artwork_id TEXT NOT NULL, theme TEXT NOT NULL, timezone TEXT NOT NULL, render_version INTEGER NOT NULL, media_type TEXT NOT NULL, file_id TEXT NOT NULL, caption TEXT NOT NULL, PRIMARY KEY(artwork_id, theme, timezone, render_version));
 INSERT INTO post_image_cache VALUES ('x:42','light','UTC',12,'video','old','caption')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var key, kind string
	if err := db.QueryRow(`SELECT translation_key,media_type FROM post_image_cache`).Scan(&key, &kind); err != nil || key != "en:ru" || kind != "video" {
		t.Fatalf("migration: %s %s %v", key, kind, err)
	}
	media, err := findPostImage(db, "x:42", "light", "UTC", defaultTranslationSettings())
	if err != nil || media.Type != "" {
		t.Fatalf("legacy version reused: %+v %v", media, err)
	}
}
