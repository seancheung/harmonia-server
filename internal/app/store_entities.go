package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

func nameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
func entityID(kind, key string) string {
	hash := sha256.Sum256([]byte(kind + "\x00" + key))
	return hex.EncodeToString(hash[:16])
}
func albumMembers(t Track, separators string) []string {
	raw := t.AlbumArtist
	if strings.TrimSpace(raw) == "" {
		raw = t.Artist
	}
	names := members(raw, separators)
	if len(names) == 0 {
		names = []string{"Unknown artist"}
	}
	return names
}
func artistSetKey(names []string) string {
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, nameKey(name))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	data, _ := json.Marshal(keys)
	return string(data)
}
func ensureNamed(tx *sql.Tx, table, kind, name string) (string, error) {
	key := nameKey(name)
	id := entityID(kind, key)
	_, err := tx.Exec("INSERT INTO "+table+"(id,name,name_key) VALUES(?,?,?) ON CONFLICT(name_key) DO NOTHING", id, strings.TrimSpace(name), key)
	return id, err
}
func ensureAlbum(tx *sql.Tx, t Track, separators string) error {
	if t.AlbumID == "" {
		return nil
	}
	names := albumMembers(t, separators)
	if _, err := tx.Exec("INSERT INTO albums(id,name,name_key,year,artist_key) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING", t.AlbumID, strings.TrimSpace(t.Album), nameKey(t.Album), t.Year, artistSetKey(names)); err != nil {
		return err
	}
	for i, name := range names {
		id, err := ensureNamed(tx, "artists", "artist", name)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO album_artists(album_id,artist_id,position) VALUES(?,?,?) ON CONFLICT(album_id,artist_id) DO NOTHING", t.AlbumID, id, i); err != nil {
			return err
		}
	}
	return nil
}
func persistTrack(tx *sql.Tx, t Track, separators string) error {
	if t.SourceID != "" {
		if _, err := tx.Exec("INSERT INTO sources(id,active) VALUES(?,0) ON CONFLICT(id) DO NOTHING", t.SourceID); err != nil {
			return err
		}
	}
	if err := ensureAlbum(tx, t, separators); err != nil {
		return err
	}
	if err := upsertModel(tx, "tracks", t); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM track_artists WHERE track_id=?", t.ID); err != nil {
		return err
	}
	for i, name := range members(t.Artist, separators) {
		id, err := ensureNamed(tx, "artists", "artist", name)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO track_artists(track_id,artist_id,position) VALUES(?,?,?)", t.ID, id, i); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM track_genres WHERE track_id=?", t.ID); err != nil {
		return err
	}
	for i, name := range members(t.Genre, separators) {
		id, err := ensureNamed(tx, "genres", "genre", name)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO track_genres(track_id,genre_id,position) VALUES(?,?,?)", t.ID, id, i); err != nil {
			return err
		}
	}
	return nil
}
func persistPlaylist(tx *sql.Tx, p Playlist) error {
	if err := upsertModel(tx, "playlists", playlistRow{p.ID, p.Name, p.Smart, p.Sort, p.Desc}); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM playlist_rules WHERE playlist_id=?", p.ID); err != nil {
		return err
	}
	if p.Rule != nil {
		nextID := 0
		if err := persistRule(tx, p.ID, *p.Rule, nil, 0, &nextID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM playlist_items WHERE playlist_id=?", p.ID); err != nil {
		return err
	}
	for i, id := range p.Tracks {
		if _, err := tx.Exec("INSERT INTO playlist_items(playlist_id,track_id,position) VALUES(?,?,?)", p.ID, id, i); err != nil {
			return err
		}
	}
	return nil
}
func changedRows[T any](before, after []T, id func(T) string, write func(T) error, remove func(string) error) error {
	old := make(map[string]T, len(before))
	for _, v := range before {
		old[id(v)] = v
	}
	for _, v := range after {
		key := id(v)
		previous, exists := old[key]
		if !exists || !reflect.DeepEqual(previous, v) {
			if err := write(v); err != nil {
				return err
			}
		}
		delete(old, key)
	}
	for key := range old {
		if err := remove(key); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) persistChanges(tx *sql.Tx, before, next State, metadata bool) error {
	remove := func(table string) func(string) error {
		return func(id string) error { _, err := tx.Exec("DELETE FROM "+table+" WHERE id=?", id); return err }
	}
	if err := changedRows(before.Sources, next.Sources, func(v Source) string { return v.ID }, func(v Source) error {
		if err := upsertModel(tx, "sources", v); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE sources SET active=1 WHERE id=?", v.ID)
		return err
	}, func(id string) error { _, err := tx.Exec("UPDATE sources SET active=0 WHERE id=?", id); return err }); err != nil {
		return err
	}
	if !metadata {
		old := before.Tracks
		// A separator change also rebuilds artist and genre associations.
		if before.TagSeparators != next.TagSeparators {
			old = nil
		}
		if err := changedRows(old, next.Tracks, func(v Track) string { return v.ID }, func(v Track) error { return persistTrack(tx, v, next.TagSeparators) }, remove("tracks")); err != nil {
			return err
		}
		if before.TagSeparators != next.TagSeparators {
			ids := map[string]bool{}
			for _, t := range next.Tracks {
				ids[t.ID] = true
			}
			for _, t := range before.Tracks {
				if !ids[t.ID] {
					if err := remove("tracks")(t.ID); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := changedRows(before.Playlists, next.Playlists, func(v Playlist) string { return v.ID }, func(v Playlist) error { return persistPlaylist(tx, v) }, remove("playlists")); err != nil {
		return err
	}
	if err := changedRows(before.RuleSets, next.RuleSets, func(v RuleSet) string { return v.ID }, func(v RuleSet) error {
		if _, err := tx.Exec("INSERT INTO rule_sets(id,name) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name", v.ID, v.Name); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM conversion_rules WHERE rule_set_id=?", v.ID); err != nil {
			return err
		}
		for i, r := range v.Rules {
			formats, err := json.Marshal(r.Formats)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO conversion_rules(rule_set_id,position,formats,format,bitrate_op,bitrate,sample_rate_op,sample_rate,codec,output_bitrate,output_sample_rate) VALUES(?,?,?,?,?,?,?,?,?,?,?)", v.ID, i, string(formats), r.Format, r.BitrateOp, r.Bitrate, r.SampleRateOp, r.SampleRate, r.Codec, r.OutputBitrate, r.OutputSampleRate); err != nil {
				return err
			}
		}
		return nil
	}, remove("rule_sets")); err != nil {
		return err
	}
	for key, value := range map[string]string{"cache_limit": strconv.FormatInt(next.CacheLimit, 10), "tag_separators": next.TagSeparators} {
		previous := before.TagSeparators
		if key == "cache_limit" {
			previous = strconv.FormatInt(before.CacheLimit, 10)
		}
		if previous != value {
			if _, err := tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
				return err
			}
		}
	}
	if !metadata {
		for id, value := range next.Sessions {
			if value && !before.Sessions[id] {
				if _, err := tx.Exec("INSERT INTO playback_sessions(id) VALUES(?) ON CONFLICT(id) DO NOTHING", id); err != nil {
					return err
				}
			}
		}
		for id := range before.Sessions {
			if !next.Sessions[id] {
				if _, err := tx.Exec("DELETE FROM playback_sessions WHERE id=?", id); err != nil {
					return err
				}
			}
		}
		if !reflect.DeepEqual(before.Tracks, next.Tracks) {
			for _, statement := range []string{
				"DELETE FROM albums WHERE NOT EXISTS (SELECT 1 FROM tracks WHERE tracks.album_id=albums.id)",
				"DELETE FROM artists WHERE NOT EXISTS (SELECT 1 FROM track_artists WHERE artist_id=artists.id) AND NOT EXISTS (SELECT 1 FROM album_artists WHERE artist_id=artists.id)",
				"DELETE FROM genres WHERE NOT EXISTS (SELECT 1 FROM track_genres WHERE genre_id=genres.id)",
				"DELETE FROM sources WHERE active=0 AND NOT EXISTS (SELECT 1 FROM tracks WHERE source_id=sources.id)",
			} {
				if _, err := tx.Exec(statement); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (s *Store) load() error {
	var err error
	if s.state.Sources, err = readModels[Source](s.db, "sources", "WHERE active=1 ORDER BY rowid"); err != nil {
		return err
	}
	if s.state.Tracks, err = readModels[Track](s.db, "tracks", "ORDER BY rowid"); err != nil {
		return err
	}
	playlists, err := readModels[playlistRow](s.db, "playlists", "ORDER BY rowid")
	if err != nil {
		return err
	}
	for _, p := range playlists {
		s.state.Playlists = append(s.state.Playlists, Playlist{ID: p.ID, Name: p.Name, Smart: p.Smart, Sort: p.Sort, Desc: p.Desc, Tracks: []string{}})
	}
	// Read each result set completely before issuing another query (one connection).
	each := func(query string, fn func(*sql.Rows) error) error {
		rows, err := s.db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	playlistByID := map[string]*Playlist{}
	for i := range s.state.Playlists {
		playlistByID[s.state.Playlists[i].ID] = &s.state.Playlists[i]
	}
	if err = each("SELECT playlist_id,track_id FROM playlist_items ORDER BY playlist_id,position", func(rows *sql.Rows) error {
		var pid, id string
		if err := rows.Scan(&pid, &id); err != nil {
			return err
		}
		playlistByID[pid].Tracks = append(playlistByID[pid].Tracks, id)
		return nil
	}); err != nil {
		return err
	}
	if err = loadRules(s.db, playlistByID); err != nil {
		return err
	}
	if err = each("SELECT id,name FROM rule_sets ORDER BY rowid", func(rows *sql.Rows) error {
		var r RuleSet
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return err
		}
		r.Rules = []Conversion{}
		s.state.RuleSets = append(s.state.RuleSets, r)
		return nil
	}); err != nil {
		return err
	}
	rulesByID := map[string]*RuleSet{}
	for i := range s.state.RuleSets {
		rulesByID[s.state.RuleSets[i].ID] = &s.state.RuleSets[i]
	}
	if err = each("SELECT rule_set_id,formats,format,bitrate_op,bitrate,sample_rate_op,sample_rate,codec,output_bitrate,output_sample_rate FROM conversion_rules ORDER BY rule_set_id,position", func(rows *sql.Rows) error {
		var id, formats string
		var r Conversion
		if err := rows.Scan(&id, &formats, &r.Format, &r.BitrateOp, &r.Bitrate, &r.SampleRateOp, &r.SampleRate, &r.Codec, &r.OutputBitrate, &r.OutputSampleRate); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(formats), &r.Formats); err != nil {
			return err
		}
		rulesByID[id].Rules = append(rulesByID[id].Rules, r)
		return nil
	}); err != nil {
		return err
	}

	if err = each("SELECT id FROM playback_sessions", func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.state.Sessions[id] = true
		return nil
	}); err != nil {
		return err
	}
	return each("SELECT key,value FROM settings", func(rows *sql.Rows) error {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		switch key {
		case "cache_limit":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid cache limit: %w", err)
			}
			s.state.CacheLimit = n
		case "tag_separators":
			s.state.TagSeparators = value
		}
		return nil
	})
}

func persistRule(tx *sql.Tx, playlistID string, rule Rule, parent any, position int, nextID *int) error {
	id := *nextID
	*nextID++
	value, err := json.Marshal(rule.Value)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO playlist_rules(playlist_id,node_id,parent_id,position,mode,field,op,value_json,source_id,recursive) VALUES(?,?,?,?,?,?,?,?,?,?)", playlistID, id, parent, position, rule.Mode, rule.Field, rule.Op, string(value), rule.SourceID, rule.Recursive); err != nil {
		return err
	}
	for i, child := range rule.Rules {
		if err := persistRule(tx, playlistID, child, id, i, nextID); err != nil {
			return err
		}
	}
	return nil
}
func loadRules(db sqlReader, playlists map[string]*Playlist) error {
	type node struct {
		id     int
		parent sql.NullInt64
		rule   Rule
	}
	rows, err := db.Query("SELECT playlist_id,node_id,parent_id,mode,field,op,value_json,source_id,recursive FROM playlist_rules ORDER BY playlist_id,position,node_id")
	if err != nil {
		return err
	}
	defer rows.Close()
	nodes := map[string][]node{}
	for rows.Next() {
		var id, value string
		var n node
		if err := rows.Scan(&id, &n.id, &n.parent, &n.rule.Mode, &n.rule.Field, &n.rule.Op, &value, &n.rule.SourceID, &n.rule.Recursive); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(value), &n.rule.Value); err != nil {
			return err
		}
		nodes[id] = append(nodes[id], n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, list := range nodes {
		if playlists[id] == nil {
			continue
		}
		children := map[int][]node{}
		var root node
		for _, n := range list {
			if n.parent.Valid {
				children[int(n.parent.Int64)] = append(children[int(n.parent.Int64)], n)
			} else {
				root = n
			}
		}
		var build func(node) Rule
		build = func(n node) Rule {
			r := n.rule
			for _, child := range children[n.id] {
				r.Rules = append(r.Rules, build(child))
			}
			return r
		}
		rule := build(root)
		playlists[id].Rule = &rule
	}
	return nil
}
