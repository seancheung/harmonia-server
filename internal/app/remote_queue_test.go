package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

type queueTestSession struct {
	*testAirplaySession
	frames        []uint64
	metadataError error
}

func (s *queueTestSession) MetadataAt(m airplay.TrackMetadata, frame uint64) error {
	s.frames = append(s.frames, frame)
	return s.metadataError
}
func (s *queueTestSession) ArtworkAt([]byte, uint64) error { return nil }

func queueReaderFixture(t *testing.T) (*remoteQueueReader, *queueTestSession, []byte, *int) {
	t.Helper()
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Tracks = []Track{{ID: "a", Duration: 3.0 / 44100}, {ID: "b", Duration: 5.0 / 44100}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	firstPCM, nextPCM := bytes.Repeat([]byte{1, 2, 3, 4}, 3), bytes.Repeat([]byte{5, 6, 7, 8}, 5)
	firstSaved := RemoteSaved{Queue: []string{"a", "b"}, Repeat: "off", PlaybackSession: "first", ShuffleVisited: []string{"a"}}
	nextSaved := cloneRemoteSaved(firstSaved)
	nextSaved.Index = 1
	nextSaved.PlaybackSession = "second"
	nextSaved.ShuffleVisited = []string{"a", "b"}
	r := &Remote{app: a, saved: cloneRemoteSaved(firstSaved)}
	first := &queuedTrack{track: a.store.Tracks([]string{"a"})[0], saved: firstSaved, pcm: bytes.NewReader(firstPCM), frames: 3}
	released := new(int)
	next := &queuedTrack{track: a.store.Tracks([]string{"b"})[0], saved: nextSaved, pcm: bytes.NewReader(nextPCM), frames: 5, release: func() { *released++ }}
	prepared := &preparedTrack{done: make(chan struct{}), cancel: func() {}, track: next}
	close(prepared.done)
	s := &queueTestSession{}
	q := &remoteQueueReader{r: r, ctx: context.Background(), session: s, current: first, pending: []*queuedTrack{first}, next: prepared, skipArtwork: true}
	t.Cleanup(q.close)
	return q, s, append(firstPCM, nextPCM...), released
}

func TestRemoteQueueJoinsSamplesAndAudibleHistory(t *testing.T) {
	q, s, want, released := queueReaderFixture(t)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(q, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("inserted padding or lost samples: %x", got)
	}
	if len(s.frames) != 1 || s.frames[0] != 3 {
		t.Fatalf("boundary=%v", s.frames)
	}
	if q.r.saved.Index != 0 {
		t.Fatal("advanced state before audio was audible")
	}
	boundary := 3 * time.Second / 44100
	q.progress(boundary - 1)
	if q.r.saved.Index != 0 || q.r.app.store.Tracks([]string{"a"})[0].PlayCount != 0 {
		t.Fatal("completed first track early")
	}
	q.progress(boundary)
	if q.r.saved.Index != 1 || q.r.saved.PlaybackSession != "second" || q.r.saved.Position != 0 {
		t.Fatalf("state: %+v", q.r.saved)
	}
	q.progress(8 * time.Second / 44100)
	q.progress(8 * time.Second / 44100)
	for _, track := range q.r.app.store.Tracks([]string{"a", "b"}) {
		if track.PlayCount != 1 {
			t.Fatalf("history %s=%d", track.ID, track.PlayCount)
		}
	}
	if _, err := q.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	q.close()
	if *released != 1 {
		t.Fatalf("PCM released %d times", *released)
	}
}

func TestRemoteQueueFallbackAndCancellation(t *testing.T) {
	for _, mode := range []string{"not-ready", "timer", "changed-queue", "cancelled", "metadata-failed"} {
		t.Run(mode, func(t *testing.T) {
			q, s, want, _ := queueReaderFixture(t)
			switch mode {
			case "not-ready":
				q.next.done = make(chan struct{})
				defer close(q.next.done)
			case "timer":
				q.r.saved.Waiting = true
			case "changed-queue":
				q.r.saved.Queue = []string{"a"}
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				q.ctx = ctx
			case "metadata-failed":
				s.metadataError = errors.New("control disconnected")
			}
			got, err := io.ReadAll(q)
			if mode == "cancelled" {
				if !errors.Is(err, context.Canceled) || len(got) != 0 {
					t.Fatal(err)
				}
				return
			}
			if !bytes.Equal(got, want[:12]) {
				t.Fatalf("played unwanted successor: %x", got)
			}
			if (err != nil) != (mode == "metadata-failed") {
				t.Fatal(err)
			}
			if q.r.saved.Index != 0 {
				t.Fatal("fallback changed queue")
			}
		})
	}
}

func TestRemoteQueuePreparationDoesNotAdvanceLiveShuffle(t *testing.T) {
	q, _, _, _ := queueReaderFixture(t)
	q.r.saved.Shuffle = true
	before := cloneRemoteSaved(q.r.saved)
	p := q.r.prepareTrack(context.Background(), before)
	p.close() // The fixture has no source; failure must leave live state untouched.
	if q.r.saved.Index != before.Index || len(q.r.saved.ShuffleVisited) != 1 || q.r.saved.PlaybackSession != before.PlaybackSession {
		t.Fatal("prefetch mutated live history")
	}
}

type queuePlaybackSession struct {
	queueTestSession
	gate, ready chan struct{}
	pcm         []byte
}

func (s *queuePlaybackSession) Stream(ctx context.Context, reader io.Reader, progress func(time.Duration)) error {
	q := reader.(*remoteQueueReader)
	select {
	case <-q.next.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	close(s.ready)
	select {
	case <-s.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	var samples int64
	for {
		b := make([]byte, airplay.PCMBytes)
		n, err := io.ReadFull(reader, b)
		s.pcm = append(s.pcm, b[:n]...)
		samples += int64(n / 4)
		progress(time.Duration(samples) * time.Second / airplay.SampleRate)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func continuousFixture(t *testing.T) (*App, <-chan *queuePlaybackSession, *atomic.Int32) {
	t.Helper()
	a := testApp(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pcm"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Path: root}}
		st.Tracks = []Track{{ID: "a", SourceID: "s", Path: "pcm", Duration: 60}, {ID: "b", SourceID: "s", Path: "pcm", Duration: 60}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	active := new(atomic.Int32)
	a.remote.openPCM = func(ctx context.Context, track Track, _ string, _ RemoteSaved) (io.Reader, func(), error) {
		active.Add(1)
		value := byte(1)
		if track.ID == "b" {
			value = 2
		}
		return bytes.NewReader(bytes.Repeat([]byte{value}, 400*4)), func() { active.Add(-1) }, nil
	}
	a.remote.mu.Lock()
	a.remote.devices["speaker"] = airplay.Device{ID: "speaker"}
	a.remote.saved.Output = "speaker"
	a.remote.mu.Unlock()
	sessions := make(chan *queuePlaybackSession, 8)
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		s := &queuePlaybackSession{queueTestSession: queueTestSession{testAirplaySession: &testAirplaySession{done: make(chan struct{})}}, gate: make(chan struct{}), ready: make(chan struct{})}
		sessions <- s
		return s, nil
	}
	return a, sessions, active
}

func awaitQueueSession(t *testing.T, sessions <-chan *queuePlaybackSession) *queuePlaybackSession {
	t.Helper()
	select {
	case s := <-sessions:
		select {
		case <-s.ready:
			return s
		case <-time.After(3 * time.Second):
			t.Fatal("queue did not start")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no connection")
	}
	return nil
}

func TestRemoteContinuousPlaybackUsesOneSession(t *testing.T) {
	a, sessions, active := continuousFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	s := awaitQueueSession(t, sessions)
	close(s.gate)
	remoteWait(t, a.remote, func() bool { return a.remote.state == "stop" })
	select {
	case <-sessions:
		t.Fatal("reconnected between songs")
	default:
	}
	if len(s.frames) != 1 || s.frames[0] != 400 {
		t.Fatalf("boundaries: %v", s.frames)
	}
	want := append(bytes.Repeat([]byte{1}, 1600), bytes.Repeat([]byte{2}, 1600)...)
	if !bytes.Equal(s.pcm, want) {
		t.Fatalf("continuous PCM length=%d", len(s.pcm))
	}
	if active.Load() != 0 {
		t.Fatalf("leaked PCM handles: %d", active.Load())
	}
}

func TestRemoteContinuousControlCancelsPrefetch(t *testing.T) {
	for _, action := range []string{"pause", "seek", "next", "timer", "clear", "append", "move", "remove", "repeat", "shuffle"} {
		t.Run(action, func(t *testing.T) {
			a, sessions, active := continuousFixture(t)
			remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
			s := awaitQueueSession(t, sessions)
			command := map[string]any{"action": action}
			if action == "seek" {
				command["position"] = 1000
			}
			if action == "timer" {
				command["finish"] = true
			}
			if action == "append" {
				command["ids"] = []string{"a"}
				command["position"] = 2
			}
			if action == "move" {
				command["index"] = 0
				command["position"] = 1
			}
			if action == "remove" {
				command["index"] = 1
			}
			if action == "repeat" {
				command["repeat"] = "all"
			}
			if action == "shuffle" {
				command["shuffle"] = true
			}
			remoteOK(t, a, command)
			select {
			case <-s.done:
			case <-time.After(time.Second):
				t.Fatal("old session survived command")
			}
			if action != "pause" && action != "clear" {
				awaitQueueSession(t, sessions)
				remoteOK(t, a, map[string]any{"action": "pause"})
			}
			a.remote.playback.Lock()
			a.remote.playback.Unlock()
			if active.Load() != 0 {
				t.Fatalf("leaked PCM handles: %d", active.Load())
			}
		})
	}
}

func TestRemoteQueueProgressDoesNotUndoConcurrentEdit(t *testing.T) {
	q, _, _, _ := queueReaderFixture(t)
	q.r.saved.Queue = []string{"b", "a"}
	q.r.saved.Index = 1
	q.progress(time.Second / 44100)
	if q.r.saved.Index != 1 {
		t.Fatal("progress overwrote edited queue index before cancellation")
	}
}

func TestRemoteContinuousPreparationPolicies(t *testing.T) {
	a, _, active := continuousFixture(t)
	for _, tc := range []struct {
		name, repeat string
		index        int
		shuffle      bool
		want         int
	}{
		{"sequential", "off", 0, false, 1},
		{"repeat-single", "single", 0, false, 0},
		{"repeat-all", "all", 1, false, 0},
		{"shuffle", "off", 0, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := RemoteSaved{Queue: []string{"a", "b"}, Index: tc.index, Repeat: tc.repeat, Shuffle: tc.shuffle, ShuffleVisited: []string{"a"}, PlaybackSession: "original"}
			p := a.remote.prepareTrack(context.Background(), saved)
			defer p.close()
			select {
			case <-p.done:
			case <-time.After(time.Second):
				t.Fatal("preparation did not finish")
			}
			if p.track == nil || p.track.saved.Index != tc.want || p.track.saved.PlaybackSession == saved.PlaybackSession {
				t.Fatalf("prepared: %+v", p.track)
			}
			if saved.Index != tc.index || len(saved.ShuffleVisited) != 1 {
				t.Fatal("preparation mutated input")
			}
		})
	}
	if active.Load() != 0 {
		t.Fatal("preparation leaked PCM")
	}
}
