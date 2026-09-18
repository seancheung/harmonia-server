package app

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// Writers commit changed rows atomically, then publish an immutable snapshot.
// Readers copy outside the snapshot lock; SQLite writes never block Read.
type Store struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex
	db         *sql.DB
	queryDB    *sql.DB
	state      State
	trackIndex map[string]int
}

func emptyState() State {
	return State{Sources: []Source{}, Tracks: []Track{}, Playlists: []Playlist{}, RuleSets: []RuleSet{}, CacheLimit: 5 * 1024 * 1024 * 1024, Sessions: map[string]bool{}}
}
func OpenStore(file string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return fail(err)
	}
	var legacy int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='library'").Scan(&legacy); err != nil {
		return fail(err)
	}
	if legacy != 0 {
		return fail(errors.New("legacy database is unsupported; remove the development database and initialize a new library"))
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version != 0 && version != 2 {
		return fail(fmt.Errorf("unsupported database version %d", version))
	}
	if version == 0 {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		if _, err = tx.Exec(databaseSchema()); err != nil {
			tx.Rollback()
			return fail(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
	}
	s := &Store{db: db, state: emptyState()}
	if err = s.load(); err != nil {
		return fail(err)
	}
	s.indexTracks()
	absolute, err := filepath.Abs(file)
	if err != nil {
		return fail(err)
	}
	dbPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(dbPath, "/") {
		dbPath = "/" + dbPath
	}
	uri := url.URL{Scheme: "file", Path: dbPath}
	uri.RawQuery = "mode=ro&_pragma=busy_timeout(5000)"
	s.queryDB, err = sql.Open("sqlite", uri.String())
	if err != nil {
		return fail(err)
	}
	s.queryDB.SetMaxOpenConns(2)
	if err = s.queryDB.Ping(); err != nil {
		s.queryDB.Close()
		return fail(err)
	}
	return s, nil
}
func (s *Store) indexTracks() {
	s.trackIndex = make(map[string]int, len(s.state.Tracks))
	for i, t := range s.state.Tracks {
		s.trackIndex[t.ID] = i
	}
}
func (s *Store) snapshot() State { s.mu.RLock(); defer s.mu.RUnlock(); return s.state }
func (s *Store) publish(next State, reindex bool) {
	s.mu.Lock()
	s.state = next
	if reindex {
		s.indexTracks()
	}
	s.mu.Unlock()
}
func cloneValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), cloneValue(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			out.Field(i).Set(cloneValue(v.Field(i)))
		}
		return out
	default:
		return v
	}
}
func clone[T any](v T) T { return cloneValue(reflect.ValueOf(v)).Interface().(T) }
func (s *Store) Library() State {
	st := s.snapshot()
	st.Sessions = nil
	st.Tracks = slices.Clone(st.Tracks)
	for i := range st.Tracks {
		st.Tracks[i].Lyrics = ""
		st.Tracks[i].Cover = ""
	}
	return clone(st)
}
func (s *Store) Read() State         { return clone(s.snapshot()) }
func (s *Store) CacheLimit() int64   { return s.snapshot().CacheLimit }
func (s *Store) RuleSets() []RuleSet { return clone(s.snapshot().RuleSets) }
func (s *Store) TrackState(id string) State {
	s.mu.RLock()
	st := s.state
	i, ok := s.trackIndex[id]
	s.mu.RUnlock()
	out := State{}
	if ok {
		out.Tracks = []Track{st.Tracks[i]}
		for _, source := range st.Sources {
			if source.ID == st.Tracks[i].SourceID {
				out.Sources = []Source{source}
				break
			}
		}
	}
	return clone(out)
}
func (s *Store) Tracks(ids []string) []Track {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Track, 0, len(ids))
	for _, id := range ids {
		if i, ok := s.trackIndex[id]; ok {
			out = append(out, clone(s.state.Tracks[i]))
		} else {
			out = append(out, Track{ID: id, Missing: true})
		}
	}
	return out
}
func (s *Store) Update(fn func(*State) error) error { return s.update(fn, false) }

// Metadata callbacks may edit sources/playlists/settings, but never tracks.
func (s *Store) UpdateMetadata(fn func(*State) error) error { return s.update(fn, true) }
func (s *Store) update(fn func(*State) error, metadata bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	before := s.snapshot()
	base := before
	if metadata {
		base.Tracks = nil
		base.Sessions = nil
	}
	next := clone(base)
	if metadata {
		next.Tracks = before.Tracks
		next.Sessions = before.Sessions
	}
	if err := fn(&next); err != nil {
		return err
	}
	if !metadata {
		ids := make(map[string]bool, len(next.Tracks))
		for i := range next.Tracks {
			if next.Tracks[i].Tags == nil {
				next.Tracks[i].Tags = map[string][]string{}
			}
			next.Tracks[i].AlbumID = albumKey(next.Tracks[i], next.TagSeparators)
			ids[next.Tracks[i].ID] = true
		}
		for i := range next.Playlists {
			next.Playlists[i].Tracks = slices.DeleteFunc(next.Playlists[i].Tracks, func(id string) bool { return !ids[id] })
		}
		for _, old := range before.Tracks {
			if !ids[old.ID] {
				for session := range next.Sessions {
					if strings.HasPrefix(session, old.ID+":") {
						delete(next.Sessions, session)
					}
				}
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.persistChanges(tx, before, next, metadata); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.publish(next, !metadata)
	return nil
}
func (s *Store) SetFavorite(id string, favorite bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	before := s.state
	i, ok := s.trackIndex[id]
	s.mu.RUnlock()
	if !ok || before.Tracks[i].Missing {
		return errors.New("track unavailable")
	}
	if before.Tracks[i].Favorite == favorite {
		return nil
	}
	if _, err := s.db.Exec("UPDATE tracks SET favorite=? WHERE id=?", favorite, id); err != nil {
		return err
	}
	next := before
	next.Tracks = slices.Clone(before.Tracks)
	next.Tracks[i].Favorite = favorite
	s.publish(next, false)
	return nil
}
func (s *Store) RecordPlayed(id, session string, seconds float64, completed, seeked bool, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	before := s.snapshot()
	s.mu.RLock()
	i, ok := s.trackIndex[id]
	s.mu.RUnlock()
	if !ok || before.Tracks[i].Missing {
		return errors.New("track unavailable")
	}
	t := before.Tracks[i]
	if session != "" && before.Sessions[id+":"+session] {
		return nil
	}
	if seconds < 15 && !(t.Duration > 0 && t.Duration < 15 && completed && !seeked && seconds >= t.Duration-0.25) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE tracks SET play_count=play_count+1,last_played=? WHERE id=?", at.UnixMilli(), id); err != nil {
		return err
	}
	if session != "" {
		if _, err = tx.Exec("INSERT INTO playback_sessions(id,track_id,played_at) VALUES(?,?,?)", id+":"+session, id, at.UnixMilli()); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE tracks SET last_played=0 WHERE last_played>0 AND id NOT IN (SELECT id FROM tracks WHERE last_played>0 ORDER BY last_played DESC,id LIMIT 500)"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	next := before
	next.Tracks = slices.Clone(before.Tracks)
	next.Tracks[i].PlayCount++
	next.Tracks[i].LastPlayed = at.UnixMilli()
	// Mirror the SQL retention ordering without sorting complete Track values.
	indices := []int{}
	for j, t := range next.Tracks {
		if t.LastPlayed > 0 {
			indices = append(indices, j)
		}
	}
	slices.SortFunc(indices, func(a, b int) int {
		ta, tb := next.Tracks[a], next.Tracks[b]
		if ta.LastPlayed > tb.LastPlayed {
			return -1
		}
		if ta.LastPlayed < tb.LastPlayed {
			return 1
		}
		return strings.Compare(ta.ID, tb.ID)
	})
	if len(indices) > 500 {
		for _, j := range indices[500:] {
			next.Tracks[j].LastPlayed = 0
		}
	}
	if session != "" {
		next.Sessions = maps.Clone(before.Sessions)
		next.Sessions[id+":"+session] = true
	}
	s.publish(next, false)
	return nil
}
func (s *Store) ClearRecent() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.Exec("UPDATE tracks SET last_played=0 WHERE last_played>0"); err != nil {
		return err
	}
	next := s.snapshot()
	next.Tracks = slices.Clone(next.Tracks)
	for i := range next.Tracks {
		next.Tracks[i].LastPlayed = 0
	}
	s.publish(next, false)
	return nil
}
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return errors.Join(s.queryDB.Close(), s.db.Close())
}
