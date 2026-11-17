package main

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestObsoleteXPostFormatPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := legacy.Exec(`CREATE TABLE x_post_rich_text_opt_in (user_id INTEGER PRIMARY KEY);
 INSERT INTO x_post_rich_text_opt_in VALUES (1);
 CREATE TABLE x_post_rich_text_opt_out (user_id INTEGER PRIMARY KEY);
 INSERT INTO x_post_rich_text_opt_out VALUES (2)`); err != nil {
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

	var count int
	row := db.QueryRow(`SELECT count(*) FROM sqlite_master
        WHERE type='table' AND name IN ('x_post_rich_text_opt_in', 'x_post_rich_text_opt_out')`)
	if err := row.Scan(&count); err != nil || count != 0 {
		t.Fatalf("obsolete tables remaining: %d, %v", count, err)
	}

	api := &instantTelegram{t: t}
	server := httptest.NewServer(api)
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := &instantSettingsScenario{t: t, db: db, bot: bot, api: api}
	s.open()
	if strings.Contains(api.snapshot().markup, "format") {
		t.Fatalf("obsolete settings button: %s", api.snapshot().markup)
	}
}
