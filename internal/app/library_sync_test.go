package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLibraryIncrementalSync(t *testing.T) {
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Tracks = []Track{{ID: "a", Title: "A", Duration: 60, Lyrics: "private lyrics", Cover: "private path"}, {ID: "b", Title: "B"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	initial := a.store.LibrarySnapshot()
	if initial.SyncCursor == "" || initial.Tracks[0].Lyrics != "" || initial.Tracks[0].Cover != "" {
		t.Fatal("invalid snapshot")
	}
	unchanged := a.store.LibraryChanges(initial.SyncCursor)
	if unchanged.Reset || len(unchanged.Tracks) != 0 || unchanged.Metadata != nil {
		t.Fatal("unexpected unchanged payload")
	}
	if err := a.store.SetFavorite("a", true); err != nil {
		t.Fatal(err)
	}
	favorite := a.store.LibraryChanges(initial.SyncCursor)
	if len(favorite.Tracks) != 1 || !favorite.Tracks[0].Favorite || favorite.Metadata != nil || favorite.Tracks[0].Lyrics != "" {
		t.Fatalf("favorite delta: %+v", favorite)
	}
	if err := a.store.RecordPlayed("a", "session", 16, false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	played := a.store.LibraryChanges(favorite.SyncCursor)
	if len(played.Tracks) != 1 || played.Tracks[0].PlayCount != 1 {
		t.Fatal("missing statistics delta")
	}
	if err := a.store.ClearRecent(); err != nil {
		t.Fatal(err)
	}
	cleared := a.store.LibraryChanges(played.SyncCursor)
	if len(cleared.Tracks) != 1 || cleared.Tracks[0].LastPlayed != 0 {
		t.Fatal("missing cleared history delta")
	}
	if err := a.store.UpdateMetadata(func(st *State) error {
		st.Playlists = []Playlist{{ID: "p", Name: "P", Tracks: []string{"a"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	metadata := a.store.LibraryChanges(cleared.SyncCursor)
	if metadata.Metadata == nil || len(metadata.Tracks) != 0 || len(metadata.Metadata.Playlists) != 1 {
		t.Fatal("missing metadata delta")
	}
	if err := a.store.Update(func(st *State) error {
		st.Tracks = []Track{st.Tracks[0], {ID: "c", Title: "New"}}
		st.Tracks[0].Title = "Changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delta := a.store.LibraryChanges(metadata.SyncCursor)
	if len(delta.Tracks) != 2 || len(delta.Removed) != 1 || delta.Removed[0] != "b" {
		t.Fatalf("scan delta: %+v", delta)
	}
	// A client that skipped intermediate updates receives the latest values once.
	cumulative := a.store.LibraryChanges(initial.SyncCursor)
	if len(cumulative.Tracks) != 2 || !cumulative.Tracks[0].Favorite || cumulative.Metadata == nil {
		t.Fatal("invalid cumulative delta")
	}
	for _, cursor := range []string{"", "bad", "other:1", a.store.syncEpoch + ":999999999"} {
		if !a.store.LibraryChanges(cursor).Reset {
			t.Fatalf("accepted invalid cursor %q", cursor)
		}
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/library/changes?since="+delta.SyncCursor, nil))
	var response LibraryChanges
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Reset || len(response.Tracks) != 0 || response.SyncCursor != delta.SyncCursor {
		t.Fatal(w.Body.String())
	}
	// Returned data cannot mutate the server's immutable snapshot.
	cumulative.Tracks[0].Title = "external"
	cumulative.Metadata.Playlists[0].Name = "external"
	if a.store.LibrarySnapshot().Tracks[0].Title == "external" || a.store.Playlists("all")[0].Name == "external" {
		t.Fatal("snapshot alias")
	}
}

func TestLibrarySyncRestartRequiresReset(t *testing.T) {
	a := testApp(t)
	b := testApp(t)
	if !b.store.LibraryChanges(a.store.LibrarySnapshot().SyncCursor).Reset {
		t.Fatal("accepted cursor from another server epoch")
	}
}

func TestLibrarySyncLargeLibraryAndNoOp(t *testing.T) {
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		for i := 0; i < 10000; i++ {
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), Title: "Example song", Artist: "Example artist", Album: "Example album"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := a.store.LibrarySnapshot()
	if err := a.store.UpdateMetadata(func(st *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if a.store.LibraryChanges(snapshot.SyncCursor).SyncCursor != snapshot.SyncCursor {
		t.Fatal("no-op changed the cursor")
	}
	if err := a.store.SetFavorite("5000", true); err != nil {
		t.Fatal(err)
	}
	delta := a.store.LibraryChanges(snapshot.SyncCursor)
	if len(delta.Tracks) != 1 || delta.Tracks[0].ID != "5000" || delta.Metadata != nil {
		t.Fatal("single edit returned unrelated data")
	}
	fullJSON, _ := json.Marshal(snapshot)
	deltaJSON, _ := json.Marshal(delta)
	if len(deltaJSON)*100 >= len(fullJSON) {
		t.Fatal("delta is unexpectedly large")
	}
	t.Logf("10,000 tracks: snapshot=%d bytes; one-song delta=%d bytes", len(fullJSON), len(deltaJSON))
}
