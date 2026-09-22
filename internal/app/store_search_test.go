package app

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIndexedQueryPlans(t *testing.T) {
	s := queryFixture(t)
	explain := func(query string, args ...any) string {
		t.Helper()
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		details := []string{}
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(details, "\n")
	}
	for _, tc := range []struct {
		rule  Rule
		index string
	}{
		{Rule{Field: "year", Op: "eq", Value: 2020}, "tracks_year"},
		{Rule{Field: "artist", Op: "eq", Value: "Artist"}, "track_search_artist"},
		{Rule{Field: "genre", Op: "eq", Value: "Rock"}, "track_search_genre"},
		{Rule{Field: "bpm", Op: "gte", Value: 120}, "track_search_bpm"},
		{Rule{Field: "key", Op: "eq", Value: "F#m"}, "track_search_key"},
		{Rule{Field: "tag:mood", Op: "eq", Value: "calm"}, "track_tag_lookup"},
		{Rule{Field: "tag:mood", Op: "isNotEmpty"}, "track_tag_nonempty"},
	} {
		where, args, err := trackWhere(Query{Rule: &tc.rule})
		if err != nil {
			t.Fatal(err)
		}
		plan := explain("SELECT t.id FROM "+trackQueryFrom+" WHERE "+where, args...)
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, "json_each") {
			t.Fatalf("%s: %s", tc.rule.Field, plan)
		}
	}
	for _, field := range []string{"addedAt", "title"} {
		for _, desc := range []bool{true, false} {
			plan := explain("SELECT t.id FROM " + queryFrom(Query{Sort: field, Desc: desc}) + " WHERE t.missing=0 ORDER BY " + orderSQL(field, desc) + " LIMIT 50")
			if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "track_search_order_") {
				t.Fatalf("sort %s/%t: %s", field, desc, plan)
			}
		}
	}
	if plan := explain("SELECT id FROM playback_sessions WHERE track_id=?", "a"); !strings.Contains(plan, "playback_sessions_track") {
		t.Fatal(plan)
	}
}

func TestDatabasePaginationOnlyDecodesRequestedRows(t *testing.T) {
	s := queryFixture(t)
	if err := s.Update(func(st *State) error {
		st.Tracks = nil
		st.Playlists = nil
		for i := 0; i < 80; i++ {
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), Title: fmt.Sprintf("Song %03d", i)})
		}
		st.Playlists = []Playlist{{ID: "manual", Tracks: []string{"3", "2", "1"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Valid JSON but invalid Track DTO at an unrequested row: pagination must not
	// decode it. A LIMIT applied after readModels would fail this request.
	if _, err := s.db.Exec(`UPDATE tracks SET tags='{"bad":3}' WHERE id='79'`); err != nil {
		t.Fatal(err)
	}
	result, err := s.QueryTrackPage(context.Background(), Query{Sort: "title", Page: 2, PageSize: 25})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 80 || len(result.Items) != 25 || result.Items[0].ID != "25" || result.Items[24].ID != "49" {
		t.Fatalf("%+v", result)
	}
	result, err = s.QueryTrackPage(context.Background(), Query{Page: int(^uint(0) >> 1), PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 80 || len(result.Items) != 0 {
		t.Fatal("out of range page")
	}
	result, err = s.QueryTrackPage(context.Background(), Query{PlaylistID: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || result.Page != 1 || result.PageSize != 50 || !reflect.DeepEqual(trackIDs(result.Items), []string{"3", "2", "1"}) {
		t.Fatalf("%+v", result)
	}
}

func TestSearchProjectionLifecycle(t *testing.T) {
	s := queryFixture(t)
	if err := s.Update(func(st *State) error {
		st.Tracks[0].Tags = map[string][]string{"mood": {"NEW", "new", ""}, "tempo": {"101"}}
		st.Tracks[0].Artist = "Changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []Rule{{Field: "tag:mood", Op: "eq", Value: "new"}, {Field: "bpm", Op: "eq", Value: 101}, {Field: "artist", Op: "eq", Value: "changed"}} {
		tracks, err := s.QueryTracks(context.Background(), Query{Rule: &rule})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(trackIDs(tracks), []string{"a"}) {
			t.Fatalf("%+v: %v", rule, trackIDs(tracks))
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER no_favorite_reindex BEFORE UPDATE ON track_search BEGIN SELECT RAISE(ABORT,'favorite reindexed metadata'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite("a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM tracks WHERE id='a'"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"track_search", "track_tag_index"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table + " WHERE track_id='a'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
}

func TestExistingLibrarySearchBackfill(t *testing.T) {
	file := filepath.Join(t.TempDir(), "existing.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *State) error {
		st.Tracks = []Track{{ID: "one", Tags: map[string][]string{"mood": {"Calm"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TABLE track_tag_index; DROP TABLE track_search"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	tracks, err := s.QueryTracks(context.Background(), Query{Rule: &Rule{Field: "tag:mood", Op: "eq", Value: "calm"}})
	if err != nil || len(tracks) != 1 {
		t.Fatalf("%v %v", tracks, err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER no_restart_reindex BEFORE INSERT ON track_search BEGIN SELECT RAISE(ABORT,'unnecessary reindex'); END`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSelectedSmartMemberships(t *testing.T) {
	s := queryFixture(t)
	// An unrelated invalid rule must not be evaluated by a selected refresh.
	if _, err := s.db.Exec("UPDATE playlist_rules SET field='unknown' WHERE playlist_id='moods'"); err != nil {
		t.Fatal(err)
	}
	result, err := s.SmartMemberships(context.Background(), "favorites")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, map[string][]string{"favorites": {"a"}}) {
		t.Fatalf("%v", result)
	}
}
