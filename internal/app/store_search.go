package app

import (
	"database/sql"
	_ "embed"
	"strings"
)

//go:embed search_schema.sql
var searchSchema string

// Install disposable lookup structures without changing the canonical schema or
// deleting the library. Backfill and index creation commit together; normal
// starts do not rebuild them. Writers maintain them in the track transaction.
func ensureSearchIndexes(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('track_search','track_tag_index')").Scan(&existing); err != nil {
		return err
	}
	if _, err = tx.Exec(searchSchema); err != nil {
		return err
	}
	if existing != 2 {
		// The two lookup tables form one projection; reconstruct both if either
		// was removed, so a surviving track_search row cannot hide missing tags.
		if _, err = tx.Exec("DELETE FROM track_search; DELETE FROM track_tag_index"); err != nil {
			return err
		}
	}
	tracks, err := readModels[Track](tx, "tracks", "WHERE id NOT IN (SELECT track_id FROM track_search)")
	if err != nil {
		return err
	}
	for _, track := range tracks {
		if err = persistSearchTrack(tx, track); err != nil {
			return err
		}
	}
	for _, field := range []string{"addedAt", "title"} {
		for _, desc := range []bool{false, true} {
			direction := "asc"
			if desc {
				direction = "desc"
			}
			order := strings.ReplaceAll(orderSQL(field, desc), "x.", "")
			if _, err = tx.Exec("CREATE INDEX IF NOT EXISTS track_search_order_" + field + "_" + direction + " ON track_search(" + order + ")"); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec("PRAGMA optimize"); err != nil {
		return err
	}
	return tx.Commit()
}

func persistSearchTrack(tx *sql.Tx, t Track) error {
	texts := []string{t.Title, t.Artist, t.Album, t.AlbumArtist, t.Genre, value(t, "key").(string)}
	args := []any{t.ID}
	for _, text := range texts {
		args = append(args, strings.ToLower(text))
	}
	args = append(args, value(t, "bpm"), t.AddedAt, t.Disc, t.Number)
	for _, text := range texts {
		args = append(args, strings.TrimSpace(text) == "")
	}
	_, err := tx.Exec(`INSERT INTO track_search VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(track_id) DO UPDATE SET
 search_title=excluded.search_title,search_artist=excluded.search_artist,
 search_album=excluded.search_album,search_album_artist=excluded.search_album_artist,
 search_genre=excluded.search_genre,search_key=excluded.search_key,search_bpm=excluded.search_bpm,
 search_added_at=excluded.search_added_at,search_disc=excluded.search_disc,search_number=excluded.search_number,
 search_title_empty=excluded.search_title_empty,search_artist_empty=excluded.search_artist_empty,
 search_album_empty=excluded.search_album_empty,search_album_artist_empty=excluded.search_album_artist_empty,
 search_genre_empty=excluded.search_genre_empty,search_key_empty=excluded.search_key_empty`, args...)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM track_tag_index WHERE track_id=?", t.ID); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO track_tag_index(track_id,tag_name,position,value_key,nonempty) VALUES(?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for name, values := range t.Tags {
		for i, text := range values {
			if _, err = stmt.Exec(t.ID, name, i, strings.ToLower(text), strings.TrimSpace(text) != ""); err != nil {
				return err
			}
		}
	}
	return nil
}
