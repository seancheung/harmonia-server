package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

func TestRemoteArtworkLifecycle(t *testing.T) {
	a, sessions := remoteFixture(t)
	a.config.AirPlayArtwork = true
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

func TestRemoteArtworkDisconnectRetriesWithoutArtwork(t *testing.T) {
	a, sessions := remoteFixture(t)
	a.config.AirPlayArtwork = true
	calls := 0
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		calls++
		s := &testAirplaySession{done: make(chan struct{}), finish: make(chan struct{}), started: make(chan struct{})}
		// After the first track, every new session rejects artwork.
		// Recovery must omit even image/none.
		if calls > 1 {
			s.artworkError = airplay.ErrArtworkTransport
		}
		sessions <- s
		return s, nil
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "index": 0})
	playing := awaitSession(t, sessions)
	close(playing.finish) // Natural EOF advances to the second track.
	first := <-sessions
	select {
	case <-first.done:
	case <-time.After(3 * time.Second):
		t.Fatal("broken session not closed")
	}
	select {
	case <-first.started:
		t.Fatal("streamed on broken connection")
	default:
	}
	next := awaitSession(t, sessions)
	if next.artworkCalls != 0 || next.metadata.HasArtwork {
		t.Fatal("retry sent artwork")
	}
	remoteWait(t, a.remote, func() bool { return a.remote.state == "play" && a.remote.saved.Index == 1 })
	a.remote.Close()
	if calls != 3 {
		t.Fatalf("connections = %d", calls)
	}
}

func TestRemoteArtworkRejectionStillPlays(t *testing.T) {
	a, sessions := remoteFixture(t)
	a.config.AirPlayArtwork = true
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		s := &testAirplaySession{artworkError: errors.New("RTSP 415 unsupported media"), done: make(chan struct{}), finish: make(chan struct{}), started: make(chan struct{})}
		sessions <- s
		return s, nil
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a"}, "index": 0})
	awaitSession(t, sessions)
	remoteWait(t, a.remote, func() bool { return a.remote.state == "play" })
}

func TestRemoteArtworkDisabledByDefault(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "index": 0})
	first := awaitSession(t, sessions)
	if first.artworkCalls != 0 || first.metadata.HasArtwork {
		t.Fatal("default session sent artwork")
	}
	close(first.finish)
	next := awaitSession(t, sessions)
	if next.artworkCalls != 0 || next.metadata.HasArtwork {
		t.Fatal("next track sent artwork")
	}
}

func TestAirPlayArtworkEnvironment(t *testing.T) {
	for _, value := range []string{"", "false", "0", "true", "1", "TRUE", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HARMONIA_AIRPLAY_ARTWORK", value)
			want := value == "true" || value == "1" || value == "TRUE"
			if ConfigFromEnv().AirPlayArtwork != want {
				t.Fatalf("unexpected setting for %q", value)
			}
		})
	}
}
