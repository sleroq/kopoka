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
	Photos                         []string
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
 CREATE TABLE IF NOT EXISTS history (sequence INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, artwork_id TEXT NOT NULL, UNIQUE(user_id, artwork_id));`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func saveSelection(db *sql.DB, user int64, a artwork) error {
	photos, err := json.Marshal(a.Photos)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO artwork VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET url=excluded.url, author=excluded.author,
        text=excluded.text, caption=excluded.caption, photos=excluded.photos`,
		a.ID, a.URL, a.Author, a.Text, a.Caption, string(photos))
	if err != nil {
		return err
	}
	if err = selectArtwork(tx, user, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func selectArtwork(db interface {
	Exec(string, ...any) (sql.Result, error)
}, user int64, id string) error {
	_, err := db.Exec(`INSERT INTO history(user_id, artwork_id) SELECT ?, id FROM artwork WHERE id = ?
        ON CONFLICT(user_id, artwork_id) DO UPDATE SET
        sequence = (SELECT coalesce(max(sequence),0)+1 FROM history)`, user, id)
	return err
}

func scanArtwork(rows *sql.Rows) (artwork, error) {
	var a artwork
	var photos string
	if err := rows.Scan(&a.ID, &a.URL, &a.Author, &a.Text, &a.Caption, &photos); err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(photos), &a.Photos); err != nil {
		return a, err
	}
	if len(a.Photos) == 0 {
		return a, fmt.Errorf("artwork %s has no photos", a.ID)
	}
	return a, nil
}

func findArtwork(db *sql.DB, id string) (*artwork, error) {
	rows, err := db.Query(`SELECT id,url,author,text,caption,photos FROM artwork WHERE id = ?`, id)
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
	rows, err := db.Query(`SELECT a.id,a.url,a.author,a.text,a.caption,a.photos
        FROM history h JOIN artwork a ON a.id=h.artwork_id
        WHERE h.user_id=? ORDER BY h.sequence DESC LIMIT 51 OFFSET ?`, user, offset)
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
