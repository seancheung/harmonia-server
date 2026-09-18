package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRelationalEntitiesAndIdentity(t *testing.T) {
	file := filepath.Join(t.TempDir(), "harmonia.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Name: "Music", Path: "/music"}}
		st.Tracks = []Track{
			{ID: "one", SourceID: "source", Album: " Blue ", Year: 2020, AlbumArtist: "One; Two", Artist: " ONE ", Genre: "Pop; Rock", Tags: tagArrays(map[string]string{"custom": "value"})},
			{ID: "two", SourceID: "source", Album: "blue", Year: 2020, AlbumArtist: "two;ONE", Artist: "one", Genre: "pop"},
			{ID: "reissue", SourceID: "source", Album: "blue", Year: 2021, AlbumArtist: "One;Two", Artist: "One"},
			{ID: "other", SourceID: "source", Album: "blue", Year: 2020, AlbumArtist: "Other", Artist: "Other"},
			{ID: "single", SourceID: "source", Artist: "One"},
		}
		st.Playlists = []Playlist{{ID: "manual", Name: "Manual", Tracks: []string{"two", "one"}}, {ID: "smart", Name: "Favorites", Smart: true, Rule: &Rule{Mode: "all", Rules: []Rule{{Field: "favorite", Op: "eq", Value: true}, {Mode: "any", Rules: []Rule{{Field: "playCount", Op: "gt", Value: 2}, {Field: "genre", Op: "eq", Value: "Pop"}}}}}}}
		st.RuleSets = []RuleSet{{ID: "rules", Name: "Rules", Rules: []Conversion{{Codec: "mp3", OutputBitrate: 192}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	count := func(query string, want int) {
		t.Helper()
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d, err %v", query, got, want, err)
		}
	}
	count("SELECT count(*) FROM albums", 3)
	count("SELECT count(*) FROM artists", 3)
	count("SELECT count(*) FROM genres", 2)
	count("SELECT count(*) FROM track_genres WHERE track_id='one'", 2)
	count("SELECT count(*) FROM tracks WHERE album_id IS NULL", 1)
	count("SELECT count(*) FROM sqlite_master WHERE name IN ('library','favorites')", 0)
	count("SELECT count(*) FROM pragma_foreign_key_check", 0)
	tracks := s.Read().Tracks
	if tracks[0].AlbumID != tracks[1].AlbumID || tracks[0].AlbumID == tracks[2].AlbumID || tracks[0].AlbumID == tracks[3].AlbumID {
		t.Fatal("album uniqueness is incorrect")
	}
	if _, err = s.db.Exec("UPDATE tracks SET album_id='nonexistent' WHERE id='one'"); err == nil {
		t.Fatal("foreign keys not enforced")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	state := s.Read()
	if rule := state.Playlists[1].Rule; rule == nil || len(rule.Rules) != 2 || len(rule.Rules[1].Rules) != 2 || rule.Rules[1].Rules[0].Value != float64(2) {
		t.Fatal("nested rule order/value was not preserved")
	}
	if len(state.Tracks) != 5 || tagText(state.Tracks[0].Tags, "custom") != "value" || state.Playlists[0].Tracks[0] != "two" || state.Playlists[1].Rule == nil || state.RuleSets[0].Rules[0].OutputBitrate != 192 {
		t.Fatal("relational round trip failed")
	}
	// Source deletion keeps missing track references valid without resurrecting it.
	if err = s.Update(func(st *State) error {
		st.Sources = nil
		for i := range st.Tracks {
			st.Tracks[i].Missing = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	count("SELECT count(*) FROM pragma_foreign_key_check", 0)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Read().Sources) != 0 || !s.Read().Tracks[0].Missing {
		t.Fatal("deleted source restored")
	}
}

func TestMetadataAndFavoritesDoNotRewriteTracks(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "harmonia.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(st *State) error { st.Tracks = []Track{{ID: "one"}, {ID: "two"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_other_tracks BEFORE UPDATE ON tracks WHEN OLD.id!='one' BEGIN SELECT RAISE(ABORT,'unrelated track write'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.SetFavorite("one", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_all_tracks BEFORE UPDATE ON tracks BEGIN SELECT RAISE(ABORT,'track write during metadata change'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateMetadata(func(st *State) error {
		st.Playlists = append(st.Playlists, Playlist{ID: "p", Smart: true, Rule: &Rule{Field: "favorite", Op: "eq", Value: true}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !s.Read().Tracks[0].Favorite {
		t.Fatal("metadata save lost favorite")
	}
}

func TestReadersDoNotWaitForPendingWrite(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "harmonia.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Update(func(st *State) error { close(entered); <-release; st.CacheLimit = 123; return nil })
	}()
	<-entered
	read := make(chan State, 1)
	go func() { read <- s.Read() }()
	select {
	case state := <-read:
		if state.CacheLimit == 123 {
			t.Error("uncommitted write became visible")
		}
	case <-time.After(time.Second):
		t.Error("reader blocked on writer")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Read().CacheLimit != 123 {
		t.Fatal("committed write was not published")
	}
}
