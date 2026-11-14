package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

type artwork struct {
	ID, URL, Author, Text, Caption string
	Photos, Types                  []string
}

func artworkID(link *url.URL) string {
	parts := strings.Split(link.Path, "/")
	id, _ := strconv.ParseUint(parts[len(parts)-1], 10, 64)
	site := "x"
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		site = "pixiv"
	}
	return site + ":" + strconv.FormatUint(id, 10)
}

func openArtworkDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection keeps in-memory databases shared and serializes SQLite writes.
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS artwork (id TEXT PRIMARY KEY, url TEXT NOT NULL, author TEXT NOT NULL, text TEXT NOT NULL, caption TEXT NOT NULL, photos TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS history (sequence INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, artwork_id TEXT NOT NULL, UNIQUE(user_id, artwork_id));
 CREATE TABLE IF NOT EXISTS history_opt_out (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS history_chat_hidden (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS instant_inline_settings (user_id INTEGER PRIMARY KEY, x INTEGER NOT NULL DEFAULT 0, pixiv INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	var hasOrigin int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('history') WHERE name='inline_origin'`).Scan(&hasOrigin); err != nil {
		_ = db.Close()
		return nil, err
	}
	if hasOrigin == 0 {
		if _, err := db.Exec(`ALTER TABLE history ADD COLUMN inline_origin INTEGER NOT NULL DEFAULT 1`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	var hasTypes int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('artwork') WHERE name='media_types'`).Scan(&hasTypes); err != nil {
		_ = db.Close()
		return nil, err
	}
	if hasTypes == 0 {
		if _, err := db.Exec(`ALTER TABLE artwork ADD COLUMN media_types TEXT NOT NULL DEFAULT '[]'`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

type instantInlineSettings struct {
	X, Pixiv bool
}

func loadInstantInlineSettings(db *sql.DB, user int64) (instantInlineSettings, error) {
	var settings instantInlineSettings
	row := db.QueryRow(`SELECT x, pixiv FROM instant_inline_settings WHERE user_id=?`, user)
	err := row.Scan(&settings.X, &settings.Pixiv)
	if err == sql.ErrNoRows {
		err = nil
	}
	return settings, err
}

func toggleInstantInline(db *sql.DB, user int64, action string) error {
	var query string
	switch action {
	case "x":
		query = `INSERT INTO instant_inline_settings(user_id, x) VALUES (?, 1)
			ON CONFLICT(user_id) DO UPDATE SET x = NOT x`
	case "pixiv":
		query = `INSERT INTO instant_inline_settings(user_id, pixiv) VALUES (?, 1)
			ON CONFLICT(user_id) DO UPDATE SET pixiv = NOT pixiv`
	case "all":
		query = `INSERT INTO instant_inline_settings(user_id, x, pixiv) VALUES (?, 1, 1)
			ON CONFLICT(user_id) DO UPDATE SET x = NOT (x AND pixiv), pixiv = NOT (x AND pixiv)`
	default:
		return fmt.Errorf("invalid instant inline action: %s", action)
	}
	_, err := db.Exec(query, user)
	return err
}

func saveSelection(db *sql.DB, user int64, a artwork) error {
	return saveArtwork(db, user, a, true)
}

func saveArtwork(db *sql.DB, user int64, a artwork, inline bool) error {
	photos, err := json.Marshal(a.Photos)
	if err != nil {
		return err
	}
	types, err := json.Marshal(a.Types)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO artwork (id,url,author,text,caption,photos,media_types) VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET url=excluded.url, author=excluded.author,
        text=excluded.text, caption=excluded.caption, photos=excluded.photos, media_types=excluded.media_types`,
		a.ID, a.URL, a.Author, a.Text, a.Caption, string(photos), string(types))
	if err != nil {
		return err
	}
	if err = recordArtwork(tx, user, a.ID, inline); err != nil {
		return err
	}
	return tx.Commit()
}

func selectArtwork(db interface {
	Exec(string, ...any) (sql.Result, error)
}, user int64, id string) error {
	return recordArtwork(db, user, id, true)
}

func recordArtwork(db interface {
	Exec(string, ...any) (sql.Result, error)
}, user int64, id string, inline bool) error {
	_, err := db.Exec(`INSERT INTO history(user_id, artwork_id, inline_origin) SELECT ?, id, ? FROM artwork WHERE id = ? AND NOT EXISTS (SELECT 1 FROM history_opt_out WHERE user_id = ?)
        ON CONFLICT(user_id, artwork_id) DO UPDATE SET
        inline_origin = max(history.inline_origin, excluded.inline_origin),
        sequence = (SELECT coalesce(max(sequence),0)+1 FROM history)`, user, inline, id, user)
	return err
}

func setHistoryEnabled(db *sql.DB, user int64, enabled bool) error {
	if enabled {
		_, err := db.Exec(`DELETE FROM history_opt_out WHERE user_id = ?`, user)
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO history_opt_out(user_id) VALUES (?)`, user)
	return err
}

func historyCount(db *sql.DB, user int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT count(*) FROM history WHERE user_id = ?`, user).Scan(&count)
	return count, err
}

func deleteHistory(db *sql.DB, user int64) error {
	_, err := db.Exec(`DELETE FROM history WHERE user_id = ?`, user)
	return err
}

func scanArtwork(rows *sql.Rows) (artwork, error) {
	var a artwork
	var photos, types string
	if err := rows.Scan(&a.ID, &a.URL, &a.Author, &a.Text, &a.Caption, &photos, &types); err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(photos), &a.Photos); err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(types), &a.Types); err != nil {
		return a, err
	}
	if len(a.Types) == 0 {
		a.Types = make([]string, len(a.Photos))
		for i := range a.Types {
			a.Types[i] = "photo"
		}
	}
	if len(a.Types) != len(a.Photos) {
		return a, fmt.Errorf("artwork %s has mismatched media types", a.ID)
	}
	if len(a.Photos) == 0 {
		return a, fmt.Errorf("artwork %s has no photos", a.ID)
	}
	return a, nil
}

func findArtwork(db *sql.DB, id string) (*artwork, error) {
	rows, err := db.Query(`SELECT id,url,author,text,caption,photos,media_types FROM artwork WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	a, err := scanArtwork(rows)
	return &a, err
}

func artworkHistory(db *sql.DB, user int64, offset int) ([]artwork, error) {
	rows, err := db.Query(`SELECT a.id,a.url,a.author,a.text,a.caption,a.photos,a.media_types
        FROM history h JOIN artwork a ON a.id=h.artwork_id
        WHERE h.user_id=? AND (h.inline_origin=1 OR NOT EXISTS (SELECT 1 FROM history_chat_hidden WHERE user_id=h.user_id)) ORDER BY h.sequence DESC LIMIT 51 OFFSET ?`, user, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []artwork
	for rows.Next() {
		a, err := scanArtwork(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
