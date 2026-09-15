package app

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
)

// All mutations commit one SQLite transaction before becoming visible to readers.
// Original music files are never opened for writing.
type Store struct {
	mu    sync.RWMutex
	db    *sql.DB
	state State
}

func OpenStore(file string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(file), 0755); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", file)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS library (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL);`); e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{db: db, state: State{Sources: []Source{}, Tracks: []Track{}, Playlists: []Playlist{}, RuleSets: []RuleSet{}, CacheLimit: 5 * 1024 * 1024 * 1024, Sessions: map[string]bool{}}}
	var data []byte
	e = db.QueryRow("SELECT data FROM library WHERE id=1").Scan(&data)
	if e == nil {
		e = json.Unmarshal(data, &s.state)
	} else if e == sql.ErrNoRows {
		e = nil
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Read() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.state)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var next State
	_ = json.Unmarshal(b, &next)
	if e := fn(&next); e != nil {
		return e
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if _, e = s.db.Exec("INSERT INTO library(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(b)); e != nil {
		return e
	}
	s.state = next
	return nil
}
func (s *Store) Close() error { return s.db.Close() }
