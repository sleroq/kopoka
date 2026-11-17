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

const thumbnailRenderVersion = 9

type artwork struct {
	ID, URL, Author, Text, Caption string
	Photos, Types                  []string
	PreviewWidth, PreviewHeight    int
}

func artworkID(link *url.URL) string {
	index := xPhotoIndex(link)
	parts := strings.Split(xPostLink(link).Path, "/")
	id, _ := strconv.ParseUint(parts[len(parts)-1], 10, 64)
	site := "x"
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		site = "pixiv"
		index = pixivPhotoIndex(link)
	}
	result := site + ":" + strconv.FormatUint(id, 10)
	if index > 0 {
		result += ":photo:" + strconv.Itoa(index)
	}
	return result
}

func openArtworkDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection keeps in-memory databases shared and serializes SQLite writes.
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS artwork (id TEXT PRIMARY KEY, url TEXT NOT NULL, author TEXT NOT NULL, text TEXT NOT NULL, caption TEXT NOT NULL, photos TEXT NOT NULL, preview_width INTEGER NOT NULL DEFAULT 0, preview_height INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS history (sequence INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, artwork_id TEXT NOT NULL, UNIQUE(user_id, artwork_id));
 CREATE TABLE IF NOT EXISTS photo_link_opt_out (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS x_post_rich_text_opt_in (user_id INTEGER PRIMARY KEY);
 DROP TABLE IF EXISTS x_post_rich_text_opt_out;
 CREATE TABLE IF NOT EXISTS history_opt_out (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS history_chat_hidden (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS history_first_saved (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS history_picture_view (user_id INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS thumbnail_cache (artwork_id TEXT NOT NULL, media_count INTEGER NOT NULL, file_id TEXT NOT NULL, render_version INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (artwork_id, media_count));
 CREATE TABLE IF NOT EXISTS post_image_cache (artwork_id TEXT NOT NULL, theme TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT 'Etc/GMT-3', translation_key TEXT NOT NULL DEFAULT 'en:ru', render_version INTEGER NOT NULL, media_type TEXT NOT NULL DEFAULT 'photo', file_id TEXT NOT NULL, caption TEXT NOT NULL, PRIMARY KEY (artwork_id, theme, timezone, translation_key, render_version));
 CREATE TABLE IF NOT EXISTS post_image_themes (user_id INTEGER PRIMARY KEY, light TEXT NOT NULL DEFAULT 'light', dark TEXT NOT NULL DEFAULT 'dark');
 CREATE TABLE IF NOT EXISTS user_translation (user_id INTEGER PRIMARY KEY, target TEXT NOT NULL DEFAULT 'en', ignored TEXT NOT NULL DEFAULT '["ru"]');
 CREATE TABLE IF NOT EXISTS user_timezone (user_id INTEGER PRIMARY KEY, timezone TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS instant_inline_settings (user_id INTEGER PRIMARY KEY, x INTEGER NOT NULL DEFAULT 0, pixiv INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migratePostImageTimezone(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migratePostImageMedia(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migratePostImageTranslation(db); err != nil {
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

	var hasFirstSequence int
	row := db.QueryRow(`SELECT count(*) FROM pragma_table_info('history') WHERE name='first_sequence'`)
	if err := row.Scan(&hasFirstSequence); err != nil {
		_ = db.Close()
		return nil, err
	}
	if hasFirstSequence == 0 {
		if _, err := db.Exec(`ALTER TABLE history ADD COLUMN first_sequence INTEGER NOT NULL DEFAULT 0;
            UPDATE history SET first_sequence = sequence`); err != nil {
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
	for _, column := range []string{"preview_width", "preview_height"} {
		var exists int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('artwork') WHERE name=?`, column).Scan(&exists); err != nil {
			_ = db.Close()
			return nil, err
		}
		if exists == 0 {
			if _, err := db.Exec(`ALTER TABLE artwork ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	var hasThumbnailVersion int
	row = db.QueryRow(`SELECT count(*) FROM pragma_table_info('thumbnail_cache') WHERE name='render_version'`)
	if err := row.Scan(&hasThumbnailVersion); err != nil {
		_ = db.Close()
		return nil, err
	}
	if hasThumbnailVersion == 0 {
		if _, err := db.Exec(`ALTER TABLE thumbnail_cache ADD COLUMN render_version INTEGER NOT NULL DEFAULT 0`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

func migratePostImageTranslation(db *sql.DB) error {
	var exists int
	row := db.QueryRow(`SELECT count(*) FROM pragma_table_info('post_image_cache') WHERE name='translation_key'`)
	if err := row.Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE post_image_cache RENAME TO post_image_cache_old;
 CREATE TABLE post_image_cache (artwork_id TEXT NOT NULL, theme TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT 'Etc/GMT-3', translation_key TEXT NOT NULL DEFAULT 'en:ru', render_version INTEGER NOT NULL, media_type TEXT NOT NULL DEFAULT 'photo', file_id TEXT NOT NULL, caption TEXT NOT NULL, PRIMARY KEY (artwork_id, theme, timezone, translation_key, render_version));
 INSERT INTO post_image_cache(artwork_id, theme, timezone, render_version, media_type, file_id, caption)
 SELECT artwork_id, theme, timezone, render_version, media_type, file_id, caption FROM post_image_cache_old;
 DROP TABLE post_image_cache_old;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func migratePostImageMedia(db *sql.DB) error {
	var exists int
	row := db.QueryRow(`SELECT count(*) FROM pragma_table_info('post_image_cache') WHERE name='media_type'`)
	if err := row.Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err := db.Exec(`ALTER TABLE post_image_cache ADD COLUMN media_type TEXT NOT NULL DEFAULT 'photo'`)
	return err
}

func migratePostImageTimezone(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('post_image_cache') WHERE name='timezone'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE post_image_cache RENAME TO post_image_cache_old;
 CREATE TABLE post_image_cache (artwork_id TEXT NOT NULL, theme TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT 'Etc/GMT-3', render_version INTEGER NOT NULL, file_id TEXT NOT NULL, caption TEXT NOT NULL, PRIMARY KEY (artwork_id, theme, timezone, render_version));
 INSERT INTO post_image_cache(artwork_id, theme, timezone, render_version, file_id, caption)
 SELECT artwork_id, theme, 'UTC', render_version, file_id, caption FROM post_image_cache_old;
 DROP TABLE post_image_cache_old;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func toggleHistorySetting(db *sql.DB, user int64, setting string) error {
	switch setting {
	case "history":
		var disabled bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_opt_out WHERE user_id=?)`, user).Scan(&disabled); err != nil {
			return err
		}
		return setHistoryEnabled(db, user, disabled)
	case "order":
		var firstSaved bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_first_saved WHERE user_id=?)`, user).Scan(&firstSaved); err != nil {
			return err
		}
		var err error
		if firstSaved {
			_, err = db.Exec(`DELETE FROM history_first_saved WHERE user_id=?`, user)
		} else {
			_, err = db.Exec(`INSERT INTO history_first_saved(user_id) VALUES (?)`, user)
		}
		return err
	case "chat":
		var hidden bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_chat_hidden WHERE user_id=?)`, user).Scan(&hidden); err != nil {
			return err
		}
		var err error
		if hidden {
			_, err = db.Exec(`DELETE FROM history_chat_hidden WHERE user_id=?`, user)
		} else {
			_, err = db.Exec(`INSERT INTO history_chat_hidden(user_id) VALUES (?)`, user)
		}
		return err
	default:
		return fmt.Errorf("invalid history setting: %s", setting)
	}
}

func pictureHistoryEnabled(db *sql.DB, user int64) (bool, error) {
	var enabled bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_picture_view WHERE user_id=?)`, user).Scan(&enabled)
	return enabled, err
}

func togglePictureHistory(db *sql.DB, user int64) error {
	enabled, err := pictureHistoryEnabled(db, user)
	if err != nil {
		return err
	}
	if enabled {
		_, err = db.Exec(`DELETE FROM history_picture_view WHERE user_id=?`, user)
	} else {
		_, err = db.Exec(`INSERT INTO history_picture_view(user_id) VALUES (?)`, user)
	}
	return err
}

func xPostRichTextEnabled(db *sql.DB, user int64) (bool, error) {
	var enabled bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM x_post_rich_text_opt_in WHERE user_id=?)`, user).Scan(&enabled)
	return enabled, err
}

func toggleXPostFormat(db *sql.DB, user int64) error {
	enabled, err := xPostRichTextEnabled(db, user)
	if err != nil {
		return err
	}
	if enabled {
		_, err = db.Exec(`DELETE FROM x_post_rich_text_opt_in WHERE user_id=?`, user)
	} else {
		_, err = db.Exec(`INSERT INTO x_post_rich_text_opt_in(user_id) VALUES (?)`, user)
	}
	return err
}

func preferredPhotoLink(db *sql.DB, user int64, link *url.URL) (*url.URL, error) {
	if xPhotoIndex(link) == 0 {
		return link, nil
	}
	var disabled bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM photo_link_opt_out WHERE user_id=?)`, user).Scan(&disabled)
	if err != nil {
		return nil, err
	}
	if disabled {
		return xPostLink(link), nil
	}
	return link, nil
}

func togglePhotoLinks(db *sql.DB, user int64) error {
	var disabled bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM photo_link_opt_out WHERE user_id=?)`, user).Scan(&disabled)
	if err != nil {
		return err
	}
	if disabled {
		_, err = db.Exec(`DELETE FROM photo_link_opt_out WHERE user_id=?`, user)
	} else {
		_, err = db.Exec(`INSERT INTO photo_link_opt_out(user_id) VALUES (?)`, user)
	}
	return err
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

func findThumbnail(db *sql.DB, id string, count int) (string, error) {
	var fileID string
	row := db.QueryRow(`SELECT file_id FROM thumbnail_cache WHERE artwork_id=? AND media_count=? AND render_version=?`,
		id, count, thumbnailRenderVersion)
	err := row.Scan(&fileID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return fileID, err
}

func saveThumbnail(db *sql.DB, id string, count, actual int, fileID string) error {
	_, err := db.Exec(`INSERT INTO thumbnail_cache(artwork_id, media_count, file_id, render_version)
		SELECT ?, ?, ?, ? UNION ALL SELECT ?, ?, ?, ? WHERE ? = 0
		ON CONFLICT(artwork_id, media_count) DO UPDATE SET file_id=excluded.file_id, render_version=excluded.render_version`,
		id, count, fileID, thumbnailRenderVersion, id, actual, fileID, thumbnailRenderVersion, count)
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
	_, err := db.Exec(`INSERT INTO history(user_id, artwork_id, inline_origin, first_sequence)
        SELECT ?, id, ?, max(coalesce((SELECT seq FROM sqlite_sequence WHERE name='history'), 0),
                             coalesce((SELECT max(sequence) FROM history), 0)) + 1
        FROM artwork WHERE id = ? AND NOT EXISTS (SELECT 1 FROM history_opt_out WHERE user_id = ?)
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

func removeHistoryArtwork(db *sql.DB, user int64, id string) (bool, error) {
	result, err := db.Exec(`DELETE FROM history WHERE user_id = ? AND artwork_id IN (?, ?, ?)`,
		user, id, id+":merged", id+":merged:bars")
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func scanArtwork(rows *sql.Rows) (artwork, error) {
	var a artwork
	var photos, types string
	if err := rows.Scan(&a.ID, &a.URL, &a.Author, &a.Text, &a.Caption, &photos, &types, &a.PreviewWidth, &a.PreviewHeight); err != nil {
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
	rows, err := db.Query(`SELECT id,url,author,text,caption,photos,media_types,preview_width,preview_height
        FROM artwork WHERE id = ?`, id)
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

func artworkHistory(db *sql.DB, user int64, offset int, source string) ([]artwork, error) {
	rows, err := db.Query(`SELECT a.id,a.url,a.author,a.text,a.caption,a.photos,a.media_types,a.preview_width,a.preview_height
        FROM history h JOIN artwork a ON a.id=h.artwork_id
        WHERE h.user_id=? AND (h.inline_origin=1 OR NOT EXISTS (SELECT 1 FROM history_chat_hidden WHERE user_id=h.user_id))
        AND (? = '' OR a.id LIKE ? || ':%')
        ORDER BY CASE WHEN EXISTS (SELECT 1 FROM history_first_saved WHERE user_id=h.user_id)
            THEN h.first_sequence ELSE h.sequence END DESC LIMIT 51 OFFSET ?`, user, source, source, offset)
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

func savePreviewDimensions(db *sql.DB, id string, width, height int) error {
	_, err := db.Exec(`UPDATE artwork SET preview_width=?, preview_height=? WHERE id=?`, width, height, id)
	return err
}
