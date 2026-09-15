package app

import (
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtworkBrowserCache(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	cover := filepath.Join(root, "embedded.jpg")
	f, err := os.Create(cover)
	if err != nil {
		t.Fatal(err)
	}
	err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Path: root}}
		st.Tracks = []Track{{ID: "song", SourceID: "source", Path: "song.mp3", Cover: cover, ArtworkRevision: "revision-one"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	get := func(version, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/tracks/song/cover?v="+version, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		res := httptest.NewRecorder()
		a.Handler().ServeHTTP(res, req)
		return res
	}
	first := get("revision-one", "")
	if first.Code != 200 || first.Header().Get("Content-Type") != "image/png" || !strings.Contains(first.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("invalid cache response: %d %v", first.Code, first.Header())
	}
	cached := get("revision-one", first.Header().Get("ETag"))
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal("conditional request retransmitted artwork")
	}
	for _, version := range []string{"", "old-version"} {
		if get(version, "").Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatal("unversioned artwork cached indefinitely")
		}
	}
	if err = a.store.Update(func(st *State) error { st.Tracks[0].ArtworkRevision = "revision-two"; return nil }); err != nil {
		t.Fatal(err)
	}
	changed := get("revision-two", first.Header().Get("ETag"))
	if changed.Code != 200 || changed.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("new revision did not invalidate cached artwork")
	}
	if err = os.Remove(cover); err != nil {
		t.Fatal(err)
	}
	missing := get("revision-two", "")
	if missing.Code != 404 || missing.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing artwork response cached")
	}
}
