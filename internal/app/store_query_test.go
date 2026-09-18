package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func queryFixture(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "query.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	err = s.Update(func(st *State) error {
		st.Tracks = []Track{
			{ID: "a", Title: "ÄBC %_", Artist: "Artist", Album: "Album", Genre: "Rock", Year: 2020, Duration: 128.5, Favorite: true, Path: `Music\song.mp3`, Tags: map[string][]string{"mood": {"Calm", "Happy"}, "bpm": {"bad", " 128.5 "}, "initialkey": {" ", "F#m"}, `a."');--`: {"safe"}}},
			{ID: "b", Title: "beta", Artist: "Other", Year: 2024, PlayCount: 3, Tags: map[string][]string{"mood": {"Sad"}, "bpm": {"NaN"}, "tbpm": {"95"}}},
			{ID: "c", Title: "empty", Tags: map[string][]string{"mood": {"  "}, "bpm": {"no number"}}},
			{ID: "d", Title: "none"},
			{ID: "missing", Title: "hidden", Missing: true, Favorite: true},
		}
		st.Playlists = []Playlist{
			{ID: "favorites", Smart: true, Rule: &Rule{Field: "favorite", Op: "eq", Value: true}},
			{ID: "moods", Smart: true, Rule: &Rule{Mode: "all", Rules: []Rule{{Field: "tag:mood", Op: "ne", Value: "sad"}, {Field: "tag:mood", Op: "isNotEmpty"}}}},
			{ID: "manual", Tracks: []string{"missing", "b", "a"}},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func trackIDs(tracks []Track) []string {
	out := []string{}
	for _, t := range tracks {
		out = append(out, t.ID)
	}
	return out
}

func TestSQLRuleParity(t *testing.T) {
	s := queryFixture(t)
	values := map[string]any{"title": "äbc %_", "artist": "Artist", "album": "Album", "genre": "rock", "albumArtist": "", "path": `Music\song.mp3`, "year": 2020, "duration": 128.5, "playCount": 0, "addedAt": 0, "disc": 0, "number": 0, "bitrate": 0, "sampleRate": 0, "bpm": 128.5, "key": "F#m", "favorite": true, "format": "mp3", "tag:mood": "calm", "tag:absent": "x", `tag:a."');--`: "safe"}
	for field, val := range values {
		for _, op := range []string{"eq", "ne", "gt", "gte", "lt", "lte", "contains", "notContains", "isEmpty", "isNotEmpty"} {
			rule := Rule{Field: field, Op: op, Value: val}
			if rule.Validate() != nil {
				continue
			}
			t.Run(field+"/"+op, func(t *testing.T) {
				q := Query{Rule: &rule, Sort: "title"}
				got, err := s.QueryTracks(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				want, err := queryTracks(s.Read(), q)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(trackIDs(got), trackIDs(want)) {
					t.Fatalf("SQL=%v Go=%v", trackIDs(got), trackIDs(want))
				}
			})
		}
	}
	for _, q := range []Query{{Search: "äbc %_"}, {PlaylistID: "moods"}, {PlaylistID: "manual"}, {PlaylistID: "manual", Rule: &Rule{Field: "favorite", Op: "eq", Value: true}}} {
		got, err := s.QueryTracks(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := queryTracks(s.Read(), q)
		if !reflect.DeepEqual(trackIDs(got), trackIDs(want)) {
			t.Fatalf("%+v SQL=%v Go=%v", q, trackIDs(got), trackIDs(want))
		}
	}
	for _, field := range []string{"title", "artist", "year", "playCount", "bpm", "key", "favorite", "addedAt"} {
		for _, desc := range []bool{true, false} {
			q := Query{Sort: field, Desc: desc}
			got, err := s.QueryTracks(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := queryTracks(s.Read(), q)
			if !reflect.DeepEqual(trackIDs(got), trackIDs(want)) {
				t.Fatalf("sort %s/%t SQL=%v Go=%v", field, desc, trackIDs(got), trackIDs(want))
			}
		}
	}
}

func TestSQLMembershipsAndCancellation(t *testing.T) {
	s := queryFixture(t)
	// Prove matching uses persisted rows, not the application track snapshot.
	if _, err := s.db.Exec("UPDATE tracks SET favorite=1 WHERE id='b'"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SmartMemberships(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string][]string{"favorites": {"a", "b"}, "moods": {"a"}}) {
		t.Fatalf("%v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SmartMemberships(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestSQLReadSnapshotDoesNotBlockFavorite(t *testing.T) {
	s := queryFixture(t)
	tx, err := s.queryDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before bool
	if err = tx.QueryRow("SELECT favorite FROM tracks WHERE id='a'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.SetFavorite("a", false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader blocked favorite write")
	}
	var snapshot bool
	if err = tx.QueryRow("SELECT favorite FROM tracks WHERE id='a'").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !before || !snapshot {
		t.Fatal("read transaction lost its consistent snapshot")
	}
}

// In-memory reference used only to verify SQL rule semantics.
func queryTracks(st State, q Query) ([]Track, error) {
	tracks := []Track{}
	if q.Rule != nil {
		if e := q.Rule.Validate(); e != nil {
			return nil, e
		}
	}
	manual := false
	if q.PlaylistID != "" {
		found := false
		for _, p := range st.Playlists {
			if p.ID == q.PlaylistID {
				found = true
				if p.Smart {
					q.Rule = p.Rule
					q.Sort = p.Sort
					q.Desc = p.Desc
				} else {
					manual = true
					byID := map[string]Track{}
					for _, t := range st.Tracks {
						byID[t.ID] = t
					}
					for _, id := range p.Tracks {
						if t, ok := byID[id]; ok {
							tracks = append(tracks, t)
						}
					}
				}
				break
			}
		}
		if !found {
			return nil, errors.New("playlist not found")
		}
	}
	if !manual {
		for _, t := range st.Tracks {
			if !t.Missing {
				tracks = append(tracks, t)
			}
		}
	}
	out := []Track{}
	for _, t := range tracks {
		if q.Search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{t.Title, t.Artist, t.Album, t.Genre}, " ")), strings.ToLower(q.Search)) {
			continue
		}
		if q.Rule != nil && !q.Rule.Match(t) {
			continue
		}
		out = append(out, t)
	}
	if !manual {
		sortTracks(out, q.Sort, q.Desc)
	}
	return out, nil
}
