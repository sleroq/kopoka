package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTimezoneCatalogIntegration(t *testing.T) {
	text, keyboard := settingsView(11)
	button := keyboard.InlineKeyboard[len(keyboard.InlineKeyboard)-1][0]
	if text != "Settings" || button.Text != "Timezone" || button.SwitchInlineQueryCurrentChat == nil || *button.SwitchInlineQueryCurrentChat != "tz " {
		t.Fatal(text, button)
	}

	for _, filter := range []string{"Berlin", "Europe Berlin", "Germany Berlin", "DE Berlin"} {
		results, _ := timezoneResults(&tgbotapi.InlineQuery{Query: "tz " + filter})
		found := false
		for _, result := range results {
			if result.(tgbotapi.InlineQueryResultArticle).ID == "timezone:Europe/Berlin" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing Berlin for %s", filter)
		}
	}

	seen := map[string]bool{}
	offset := ""
	for {
		results, next := timezoneResults(&tgbotapi.InlineQuery{Query: "tz ", Offset: offset})
		if len(results) > 50 {
			t.Fatal("page too large")
		}
		for _, result := range results {
			article := result.(tgbotapi.InlineQueryResultArticle)
			if seen[article.ID] {
				t.Fatalf("duplicate %s", article.ID)
			}
			seen[article.ID] = true
			if article.ReplyMarkup == nil || article.InputMessageContent.(tgbotapi.InputTextMessageContent).Text != "Setting timezone…" {
				t.Fatal(article)
			}
		}
		if next == "" {
			break
		}
		offset = next
	}
	if len(seen) < 400 {
		t.Fatal(len(seen))
	}
	filtered := map[string]bool{}
	for offset := ""; ; {
		page, next := timezoneResults(&tgbotapi.InlineQuery{Query: "tz America", Offset: offset})
		for _, result := range page {
			id := result.(tgbotapi.InlineQueryResultArticle).ID
			if filtered[id] {
				t.Fatalf("duplicate filtered result %s", id)
			}
			filtered[id] = true
		}
		if next == "" {
			break
		}
		offset = next
	}
	if len(filtered) <= 50 {
		t.Fatalf("expected multiple filtered pages, got %d", len(filtered))
	}
	zone, _ := timezoneByID(defaultTimezone)
	if zone.title() != "UTC+03:00" {
		t.Fatal(zone.title())
	}

	results, _ := timezoneResults(&tgbotapi.InlineQuery{Query: "tz ", Offset: "9999999999999999999999"})
	if len(results) != 50 {
		t.Fatal(len(results))
	}
	for _, query := range []string{"tz", "TZ ", " tz "} {
		if filter, ok := timezoneQuery(query); !ok || filter != "" {
			t.Fatalf("empty timezone filter: %q, %q, %v", query, filter, ok)
		}
	}
	for query, want := range map[string]string{"UTC+3": defaultTimezone, "GMT+3": defaultTimezone, "GMT-3": "Etc/GMT+3"} {
		results, _ := timezoneResults(&tgbotapi.InlineQuery{Query: "tz " + query})
		if len(results) != 1 || results[0].(tgbotapi.InlineQueryResultArticle).ID != timezoneResultPrefix+want {
			t.Fatalf("offset filter %q: %+v", query, results)
		}
	}
	for _, q := range []string{"tzx ", "history"} {
		if _, ok := timezoneQuery(q); ok {
			t.Fatal(q)
		}
	}
}

func TestTimezonePersistenceAndSelectionIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}

	var edit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			_ = r.ParseForm()
			edit = r.FormValue("text")
			_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}

	if got, err := loadUserTimezone(db, 11); err != nil || got != defaultTimezone {
		t.Fatal(got, err)
	}
	handleTimezoneSelection(bot, db, &tgbotapi.ChosenInlineResult{
		ResultID: "timezone:Europe/Berlin", From: &tgbotapi.User{ID: 11}, InlineMessageID: "inline",
	})
	if !strings.Contains(edit, "Europe/Berlin") {
		t.Fatal(edit)
	}
	handleTimezoneSelection(bot, db, &tgbotapi.ChosenInlineResult{ResultID: "timezone:UTC", From: &tgbotapi.User{ID: 12}})
	handleTimezoneSelection(bot, db, &tgbotapi.ChosenInlineResult{
		ResultID: "timezone:forged", From: &tgbotapi.User{ID: 11},
	})
	if err := saveUserTimezone(db, 11, "forged"); err == nil {
		t.Fatal("accepted invalid zone")
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openArtworkDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for user, want := range map[int64]string{11: "Europe/Berlin", 12: "UTC", 13: defaultTimezone} {
		got, err := loadUserTimezone(db, user)
		if err != nil || got != want {
			t.Fatal(user, got, err)
		}
	}
	if count, err := historyCount(db, 11); err != nil || count != 0 {
		t.Fatal(count, err)
	}

	_ = db.Close()
	edit = ""
	handleTimezoneSelection(bot, db, &tgbotapi.ChosenInlineResult{
		ResultID: "timezone:UTC", From: &tgbotapi.User{ID: 11}, InlineMessageID: "inline",
	})
	if !strings.Contains(edit, "Could not save") {
		t.Fatal(edit)
	}
}

func TestTimezoneCacheMigrationIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE post_image_cache (artwork_id TEXT NOT NULL, theme TEXT NOT NULL, render_version INTEGER NOT NULL, file_id TEXT NOT NULL, caption TEXT NOT NULL,
 PRIMARY KEY (artwork_id, theme, render_version));
 INSERT INTO post_image_cache VALUES ('x:1','dark',4,'file','caption')`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for range 2 {
		db, err = openArtworkDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var file, caption string
		row := db.QueryRow(`SELECT file_id,caption FROM post_image_cache WHERE artwork_id=? AND timezone=?`, "x:1", "UTC")
		err = row.Scan(&file, &caption)
		if err != nil || file != "file" || caption != "caption" {
			t.Fatal(file, caption, err)
		}
		_, err = db.Exec(`INSERT OR IGNORE INTO post_image_cache(artwork_id,theme,timezone,render_version,file_id,caption)
 VALUES ('x:1','dark','Europe/Berlin',4,'second','other')`)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err = db.QueryRow(`SELECT count(*) FROM post_image_cache`).Scan(&count); err != nil || count != 2 {
			t.Fatal(count, err)
		}
		_ = db.Close()
	}
}
