package app

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteArtworkLifecycle(t *testing.T) {
	a, sessions := remoteFixture(t)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.jpg")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *State) error { st.Tracks[0].Cover = path; return nil }); err != nil {
		t.Fatal(err)
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "index": 0, "playing": true})
	check := func() {
		t.Helper()
		s := awaitSession(t, sessions)
		if !s.metadata.HasArtwork || !bytes.Equal(s.artwork, encoded.Bytes()) {
			t.Fatal("cover not passed to sender")
		}
	}
	check()
	remoteOK(t, a, map[string]any{"action": "pause"})
	remoteOK(t, a, map[string]any{"action": "play"})
	check()
	remoteOK(t, a, map[string]any{"action": "next"})
	s := awaitSession(t, sessions)
	if s.metadata.HasArtwork || len(s.artwork) != 0 {
		t.Fatal("previous cover retained on track without artwork")
	}
}

func TestRemoteArtworkInvalidFile(t *testing.T) {
	if b, err := remoteArtwork(""); err != nil || len(b) != 0 {
		t.Fatal("missing cover should clear artwork")
	}
	path := filepath.Join(t.TempDir(), "invalid.png")
	os.WriteFile(path, []byte("bad image"), 0600)
	if _, err := remoteArtwork(path); err == nil {
		t.Fatal("accepted corrupt image")
	}
	if _, err := remoteArtwork(filepath.Dir(path)); err == nil {
		t.Fatal("accepted directory")
	}
}
