package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
)

func seedBrowse(t *testing.T) *App {
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Name: "Music"}}
		for i := 0; i < 240; i++ {
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprintf("%03d", i), SourceID: "source", Title: fmt.Sprintf("Song %03d", i), Artist: fmt.Sprintf("Artist %02d", i%12), AlbumArtist: "Album artist", Album: fmt.Sprintf("Album %02d", i%6), Year: 2012, Disc: i%2 + 1, Number: i + 1, AddedAt: int64(i + 1), PlayCount: i % 4, LastPlayed: int64(i), Folder: fmt.Sprintf("Folder %02d", i%3), Cover: "private", Lyrics: "private"})
		}
		st.Playlists = []Playlist{{ID: "normal", Name: "Normal", Tracks: []string{"200", "003", "150"}}, {ID: "smart", Name: "Smart", Smart: true, Rule: &Rule{Field: "playCount", Op: "gt", Value: 0}, Sort: "title"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}
func browseURL(path string, q BrowseQuery) string {
	data, _ := json.Marshal(q)
	return path + "?query=" + url.QueryEscape(string(data))
}
func TestBrowsePagesAndQueueLimit(t *testing.T) {
	a := seedBrowse(t)
	first := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "songs", Page: 1, PageSize: 25}), nil)
	var p BrowsePage
	if err := json.Unmarshal(first.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if first.Code != 200 || p.Total != 240 || len(p.Items) != 25 || p.Items[0].ID != "000" || p.Items[0].Cover != "" || p.Items[0].Lyrics != "" {
		t.Fatalf("bad page: %s", first.Body.String())
	}
	second := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "songs", Page: 2, PageSize: 25}), nil)
	json.Unmarshal(second.Body.Bytes(), &p)
	if p.Items[0].ID != "025" {
		t.Fatal("page overlap")
	}
	artists := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "artists", PageSize: 5}), nil)
	json.Unmarshal(artists.Body.Bytes(), &p)
	if p.Total != 13 || len(p.Groups) != 5 {
		t.Fatalf("bad artist page: %s", artists.Body.String())
	}
	for _, g := range p.Groups {
		if len(g.Tracks) > 4 {
			t.Fatal("unbounded group preview")
		}
	}
	playlist := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "playlists", Detail: "normal", PageSize: 2}), nil)
	json.Unmarshal(playlist.Body.Bytes(), &p)
	if p.Items[0].ID != "200" || p.Items[1].ID != "003" || p.Total != 3 || len(p.Playlist.Tracks) != 0 {
		t.Fatal("manual playlist order or metadata")
	}
	smart := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "playlists", Detail: "smart", PageSize: 25}), nil)
	json.Unmarshal(smart.Body.Bytes(), &p)
	if p.Total != 180 || len(p.Items) != 25 {
		t.Fatal("smart pagination")
	}
	var queue struct {
		IDs   []string `json:"ids"`
		Limit int      `json:"limit"`
	}
	res := request(t, a, "GET", browseURL("/api/queue/query", BrowseQuery{Section: "songs"}), nil)
	json.Unmarshal(res.Body.Bytes(), &queue)
	if len(queue.IDs) != 100 || queue.Limit != 100 {
		t.Fatalf("queue not capped: %s", res.Body.String())
	}
	res = request(t, a, "GET", browseURL("/api/queue/query", BrowseQuery{Section: "songs", Start: "150"}), nil)
	json.Unmarshal(res.Body.Bytes(), &queue)
	if len(queue.IDs) != 90 || queue.IDs[0] != "150" {
		t.Fatal("selected track outside first page was not included")
	}
	for _, q := range []BrowseQuery{{Section: "bad"}, {Section: "songs", PageSize: 1000}, {Section: "songs", Rule: &Rule{Field: "bad", Op: "eq", Value: "x"}}} {
		if request(t, a, "GET", browseURL("/api/browse", q), nil).Code != 400 {
			t.Fatal("invalid query accepted")
		}
	}
}
func TestPageCacheETagsAndSmallInitialization(t *testing.T) {
	a := seedBrowse(t)
	for _, path := range []string{"/api/home", "/api/config", browseURL("/api/browse", BrowseQuery{Section: "albums", PageSize: 5})} {
		res := request(t, a, "GET", path, nil)
		if res.Code != 200 || res.Header().Get("ETag") == "" {
			t.Fatal("missing ETag")
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("If-None-Match", res.Header().Get("ETag"))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		if w.Code != 304 || w.Body.Len() != 0 {
			t.Fatal("unchanged page retransmitted")
		}
	}
	config := request(t, a, "GET", "/api/config", nil)
	var st State
	json.Unmarshal(config.Body.Bytes(), &st)
	if len(st.Tracks) != 0 || len(st.Playlists) != 0 || len(st.Sources) != 1 {
		t.Fatal("configuration downloaded music library")
	}
	if err := a.store.SetFavorite("000", true); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("If-None-Match", config.Header().Get("ETag"))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)
	if w.Code != 304 {
		t.Fatal("unrelated change invalidated config contents")
	}
	var home struct {
		Recent, Frequent, Unheard []Track
		Artists                   []BrowseGroup
	}
	json.Unmarshal(request(t, a, "GET", "/api/home", nil).Body.Bytes(), &home)
	if len(home.Recent) != 8 || len(home.Frequent) != 8 || len(home.Unheard) != 8 || len(home.Artists) != 8 {
		t.Fatal("homepage preview limits")
	}
	a.config.Token = "secret"
	if request(t, a, "GET", "/api/home", nil).Code != 401 {
		t.Fatal("cache bypassed authentication")
	}
}

func TestBrowseSearchAndQueueSelection(t *testing.T) {
	a := seedBrowse(t)
	var page BrowsePage
	res := request(t, a, "GET", browseURL("/api/browse", BrowseQuery{Section: "artists", Search: "Artist 01", PageSize: 1}), nil)
	if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Groups) != 1 || page.Groups[0].Name != "Artist 01" || page.Groups[0].Count != 20 {
		t.Fatalf("group search must retain complete counts: %s", res.Body.String())
	}
	if request(t, a, "GET", browseURL("/api/queue/query", BrowseQuery{Section: "songs", Start: "gone"}), nil).Code != 404 {
		t.Fatal("missing start must not play a different song")
	}
	ids := make([]string, 201)
	if request(t, a, "POST", "/api/tracks/resolve", map[string]any{"ids": ids}).Code != 400 {
		t.Fatal("unbounded resolution accepted")
	}
}

func TestBrowseFolderSortAndSearch(t *testing.T) {
	st := State{Sources: []Source{{ID: "s", FolderTimes: map[string]FileTimes{"Alpha": {ModifiedAt: 10}, "Beta": {ModifiedAt: 20}}}}, Tracks: []Track{{ID: "a", SourceID: "s", Folder: "Alpha"}, {ID: "b", SourceID: "s", Folder: "Beta"}, {ID: "c", SourceID: "s", Folder: "Unknown"}}}
	page := buildBrowse(st, BrowseQuery{Section: "folders", Detail: "s|", Sort: "modifiedAt", Desc: true})
	if fmt.Sprint(page.Folders) != "[Beta Alpha Unknown]" {
		t.Fatalf("bad folder order: %v", page.Folders)
	}
	page = buildBrowse(st, BrowseQuery{Section: "folders", Detail: "s|", Search: "bet"})
	if fmt.Sprint(page.Folders) != "[Beta]" {
		t.Fatalf("bad folder search: %v", page.Folders)
	}
}

func TestRemovedLibraryEndpoints(t *testing.T) {
	a := testApp(t)
	for _, route := range []struct{ method, path string }{{"GET", "/api/library"}, {"GET", "/api/library/changes"}, {"POST", "/api/tracks/query"}, {"GET", "/api/playlists/memberships"}} {
		if res := request(t, a, route.method, route.path, nil); res.Code != 404 && res.Code != 405 {
			t.Fatalf("removed route %s returned %d", route.path, res.Code)
		}
	}
}
