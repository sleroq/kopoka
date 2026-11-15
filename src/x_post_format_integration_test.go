package main

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestXPostFormatSettingsFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE x_post_rich_text_opt_out (user_id INTEGER PRIMARY KEY);
		INSERT INTO x_post_rich_text_opt_out(user_id) VALUES (3)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	api := &instantTelegram{t: t}
	server := httptest.NewServer(api)
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &instantSettingsScenario{t: t, db: db, bot: bot, api: api}
	check := func(user int64, want bool) {
		t.Helper()
		got, err := xPostRichTextEnabled(db, user)
		if err != nil || got != want {
			t.Fatalf("user %d rich text = %v, %v; want %v", user, got, err, want)
		}
	}

	s.open()
	s.tap("X post format", 1)
	s.expectButtons("Telegram text", "Back")
	check(1, false)
	check(3, false)

	s.tap("Telegram text", 2)
	if !strings.Contains(api.snapshot().callback, "another user") {
		t.Fatalf("unauthorized callback: %#v", api.snapshot())
	}
	check(1, false)

	s.tap("Telegram text", 1)
	s.expectButtons("Rich text", "Back")
	check(1, true)
	check(2, false)
	s.tap("Back", 1)
	if api.snapshot().text != "Settings" {
		t.Fatalf("back: %#v", api.snapshot())
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db = db
	check(1, true)
	check(2, false)
	check(3, false)
	s.open()
	s.tap("X post format", 1)
	s.expectButtons("Rich text", "Back")
	s.tap("Rich text", 1)
	s.expectButtons("Telegram text", "Back")
	check(1, false)
}
