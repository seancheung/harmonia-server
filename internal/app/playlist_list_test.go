package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestPlaylistListFilters(t *testing.T) {
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Tracks = []Track{{ID: "track"}}
		st.Playlists = []Playlist{{ID: "normal", Name: "Normal", Tracks: []string{"track"}}, {ID: "smart", Name: "Smart", Smart: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query         string
		count, status int
	}{
		{"", 2, 200}, {"?type=all", 2, 200}, {"?type=normal", 1, 200}, {"?type=smart", 1, 200}, {"?type=invalid", 0, 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/playlists"+tc.query, nil))
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.status != 200 {
				return
			}
			var result struct {
				Playlists []Playlist `json:"playlists"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Playlists) != tc.count {
				t.Fatalf("unexpected list: %+v", result.Playlists)
			}
			if tc.query == "?type=normal" && (result.Playlists[0].Smart || len(result.Playlists[0].Tracks) != 1 || result.Playlists[0].Tracks[0] != "track") {
				t.Fatal("normal playlist mismatch")
			}
			if tc.query == "?type=smart" && !result.Playlists[0].Smart {
				t.Fatal("smart playlist mismatch")
			}
		})
	}
	if err := a.store.Update(func(st *State) error { st.Playlists = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/playlists", nil))
	var result map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if string(result["playlists"]) != "[]" {
		t.Fatalf("expected empty array: %s", w.Body.String())
	}
}
