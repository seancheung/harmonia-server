package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkIndexedLibrary(b *testing.B) {
	s, err := OpenStore(filepath.Join(b.TempDir(), "bench.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	err = s.Update(func(st *State) error {
		for i := 0; i < 8000; i++ {
			mood := "Other"
			if i%8 == 0 {
				mood = "Calm"
			}
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), Title: fmt.Sprintf("Song %05d", i), Artist: "Artist", Album: "Album", Year: 2000 + i%25, AddedAt: int64(i + 1), Tags: map[string][]string{"mood": {mood, "Extra"}, "bpm": {fmt.Sprint(80 + i%80)}, "artist": {"Artist"}, "album": {"Album"}, "comment": {"Example metadata"}}})
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, query string }{
		{"JSON tag equality", `SELECT t.id FROM tracks t WHERE t.missing=0 AND EXISTS (SELECT 1 FROM json_each(t.tags) j,json_each(j.value) v WHERE j.key='mood' AND harmonia_lower(v.value)='calm')`},
		{"Indexed tag equality", `SELECT t.id FROM tracks t WHERE t.missing=0 AND t.id IN (SELECT track_id FROM track_tag_index WHERE tag_name='mood' AND value_key='calm')`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				rows, err := s.queryDB.Query(tc.query)
				if err != nil {
					b.Fatal(err)
				}
				count := 0
				for rows.Next() {
					var id string
					if err = rows.Scan(&id); err != nil {
						b.Fatal(err)
					}
					count++
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					b.Fatal(err)
				}
				if count != 1000 {
					b.Fatal(count)
				}
			}
		})
	}
	b.Run("Decode all matches", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			tracks, err := s.QueryTracks(context.Background(), Query{Sort: "title"})
			if err != nil || len(tracks) != 8000 {
				b.Fatalf("%d %v", len(tracks), err)
			}
		}
	})
	b.Run("Database page 50", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			page, err := s.QueryTrackPage(context.Background(), Query{Sort: "title", Page: 1, PageSize: 50})
			if err != nil || len(page.Items) != 50 || page.Total != 8000 {
				b.Fatalf("%d %v", len(page.Items), err)
			}
		}
	})
}
