package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestFavoriteRelationalPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "library.db")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err := s.Update(func(st *State) error { st.Tracks = []Track{{ID: "one"}, {ID: "missing", Missing: true}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite("one", true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unknown", "missing"} {
		if s.SetFavorite(id, true) == nil {
			t.Fatalf("accepted %s", id)
		}
	}
	reopen := func() {
		t.Helper()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = OpenStore(file)
		if err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	if !s.Read().Tracks[0].Favorite {
		t.Fatal("favorite lost on reopen")
	}
	if err := s.Update(func(st *State) error { st.CacheLimit = 123; return errors.New("abort") }); err == nil {
		t.Fatal("expected failure")
	}
	reopen()
	if !s.Read().Tracks[0].Favorite || s.Read().CacheLimit == 123 {
		t.Fatal("failed update changed state")
	}
	// A SQL failure must preserve both the published state and durable rows.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_favorite_change BEFORE UPDATE ON tracks BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Tracks[0].Favorite = false; return nil }); err == nil {
		t.Fatal("expected transaction failure")
	}
	reopen()
	if !s.Read().Tracks[0].Favorite {
		t.Fatal("failed transaction lost favorite")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_favorite_change"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.CacheLimit = 456; return nil }); err != nil {
		t.Fatal(err)
	}

	reopen()
	if !s.Read().Tracks[0].Favorite {
		t.Fatal("snapshot lost favorite")
	}
	if err := s.SetFavorite("one", false); err != nil {
		t.Fatal(err)
	}
	reopen()
	if s.Read().Tracks[0].Favorite {
		t.Fatal("unfavorite lost on reopen")
	}
	// Deleting a track must not leave an override that affects a later reimport.
	if err := s.SetFavorite("one", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Tracks = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Tracks = []Track{{ID: "one"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	reopen()
	if s.Read().Tracks[0].Favorite {
		t.Fatal("deleted track retained favorite override")
	}
	// A failed database write must not change the in-memory favorite.
	s.Close()
	if s.SetFavorite("one", true) == nil {
		t.Fatal("expected closed database failure")
	}
	if s.Read().Tracks[0].Favorite {
		t.Fatal("failed write changed memory")
	}
}

func BenchmarkFavorite8000(b *testing.B) {
	for _, snapshot := range []bool{true, false} {
		name := "single_track"
		if snapshot {
			name = "batch_update"
		}
		b.Run(name, func(b *testing.B) {
			s, err := OpenStore(filepath.Join(b.TempDir(), "library.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if err := s.Update(func(st *State) error {
				for i := 0; i < 8000; i++ {
					st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), Title: "Example title", Artist: "Example artist", Album: "Example album", Lyrics: strings.Repeat("lyrics ", 100), Tags: tagArrays(map[string]string{"genre": "Pop"})})
				}
				for i := 0; i < 5; i++ {
					st.Playlists = append(st.Playlists, Playlist{ID: fmt.Sprint(i), Smart: true, Rule: &Rule{Field: "favorite", Op: "eq", Value: true}})
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				value := i%2 == 0
				var err error
				if snapshot {
					err = s.Update(func(st *State) error { st.Tracks[7999].Favorite = value; return nil })
				} else {
					err = s.SetFavorite("7999", value)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
