package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

type testAirplaySession struct {
	artwork               []byte
	metadata              airplay.TrackMetadata
	volumeWrites          []int
	progress              time.Duration
	done, finish, started chan struct{}
	once                  sync.Once
}

func (s *testAirplaySession) Close() { s.once.Do(func() { close(s.done) }) }
func (s *testAirplaySession) Volume(v int) error {
	s.volumeWrites = append(s.volumeWrites, v)
	return nil
}
func (s *testAirplaySession) Metadata(m airplay.TrackMetadata) error { s.metadata = m; return nil }
func (s *testAirplaySession) Artwork(b []byte) error {
	s.artwork = append([]byte(nil), b...)
	return nil
}
func (s *testAirplaySession) Stream(ctx context.Context, reader io.Reader, progress func(time.Duration)) error {
	if _, e := io.Copy(io.Discard, reader); e != nil {
		return e
	}
	position := s.progress
	if position == 0 {
		position = 250 * time.Millisecond
	}
	progress(position)
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return context.Canceled
	case <-s.finish:
		return nil
	}
}
func remoteFixture(t *testing.T) (*App, chan *testAirplaySession) {
	t.Helper()
	a := testApp(t)
	root := t.TempDir()
	fixture(t, filepath.Join(root, "song.wav"), "Remote")
	if e := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Path: root}}
		st.Tracks = []Track{{ID: "a", SourceID: "s", Path: "song.wav", Duration: 60}, {ID: "b", SourceID: "s", Path: "song.wav", Duration: 60}, {ID: "c", SourceID: "s", Path: "song.wav", Duration: 60}}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	a.remote.readVolume = func(context.Context, airplay.Device, string, *airplay.Credentials) (int, error) { return 23, nil }
	sessions := make(chan *testAirplaySession, 16)
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		s := &testAirplaySession{done: make(chan struct{}), finish: make(chan struct{}), started: make(chan struct{})}
		sessions <- s
		return s, nil
	}
	a.remote.mu.Lock()
	a.remote.devices["speaker"] = airplay.Device{ID: "speaker"}
	a.remote.mu.Unlock()
	remoteOK(t, a, map[string]any{"action": "outputs", "outputs": []string{"speaker"}})
	return a, sessions
}
func remoteOK(t *testing.T, a *App, b any) {
	t.Helper()
	res := request(t, a, "POST", "/api/remote", b)
	if res.Code != 200 {
		t.Fatalf("remote command: %d %s", res.Code, res.Body.String())
	}
}
func awaitSession(t *testing.T, sessions <-chan *testAirplaySession) *testAirplaySession {
	t.Helper()
	select {
	case s := <-sessions:
		select {
		case <-s.started:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("stream not started")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session not connected")
	}
	return nil
}
func remoteWait(t *testing.T, r *Remote, condition func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		r.mu.Lock()
		ok := condition()
		r.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("remote state did not converge")
}
func TestRemoteShuffleAutomaticPlayback(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "shuffle", "shuffle": true})
	remoteOK(t, a, map[string]any{"action": "repeat", "repeat": "all"})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b", "c"}, "index": 0})
	last := ""
	for round := 0; round < 2; round++ {
		seen := map[string]bool{}
		for i := 0; i < 3; i++ {
			session := awaitSession(t, sessions)
			id := session.metadata.ID
			if seen[id] || id == last {
				t.Fatalf("round %d repeated %s", round, id)
			}
			seen[id] = true
			last = id
			if round == 1 && i == 2 {
				remoteOK(t, a, map[string]any{"action": "repeat", "repeat": "off"})
			}
			close(session.finish)
		}
	}
	remoteWait(t, a.remote, func() bool { return a.remote.state == "stop" })
}

func TestRemoteSingleDevicePausedTransferAndSeek(t *testing.T) {
	a, sessions := remoteFixture(t)
	res := request(t, a, "POST", "/api/remote", map[string]any{"action": "outputs", "outputs": []string{"speaker", "other"}})
	if res.Code != 400 {
		t.Fatal("accepted multiroom")
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "index": 1, "position": 12500, "playing": false})
	select {
	case <-sessions:
		t.Fatal("paused transfer opened a session")
	default:
	}
	remoteWait(t, a.remote, func() bool {
		return a.remote.state == "pause" && a.remote.saved.Position == 12500 && a.remote.saved.Index == 1
	})
	remoteOK(t, a, map[string]any{"action": "play"})
	s := awaitSession(t, sessions)
	remoteOK(t, a, map[string]any{"action": "pause"})
	select {
	case <-s.done:
	default:
		t.Fatal("pause leaked session")
	}
	remoteOK(t, a, map[string]any{"action": "seek", "position": 20000})
	remoteWait(t, a.remote, func() bool { return a.remote.saved.Position == 20000 && a.remote.state == "pause" })
	res = request(t, a, "POST", "/api/remote", map[string]any{"action": "seek", "position": -1})
	if res.Code != 400 {
		t.Fatal("negative seek accepted")
	}
	remoteOK(t, a, map[string]any{"action": "clear"})
	remoteWait(t, a.remote, func() bool { return a.remote.state == "stop" && len(a.remote.saved.Queue) == 0 })
}
func TestRemoteQueueEditsKeepCurrentOccurrence(t *testing.T) {
	a, _ := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b", "a"}, "index": 2, "playing": false})
	remoteOK(t, a, map[string]any{"action": "append", "ids": []string{"c"}, "position": 0})
	remoteOK(t, a, map[string]any{"action": "move", "index": 3, "position": 1})
	remoteOK(t, a, map[string]any{"action": "remove", "index": 0})
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if strings.Join(a.remote.saved.Queue, ",") != "a,a,b" || a.remote.saved.Index != 0 {
		t.Fatalf("queue/index changed current occurrence: %+v", a.remote.saved)
	}
}
func TestRemoteTimerStopsAtTrackEndWithoutChangingQueue(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	s := awaitSession(t, sessions)
	remoteOK(t, a, map[string]any{"action": "repeat", "repeat": "all"})
	remoteOK(t, a, map[string]any{"action": "timer", "deadline": 0, "finish": true})
	select {
	case <-s.done:
		t.Fatal("timer interrupted track")
	default:
	}
	close(s.finish)
	remoteWait(t, a.remote, func() bool { return a.remote.state == "pause" && !a.remote.saved.Waiting })
	a.remote.mu.Lock()
	if len(a.remote.saved.Queue) != 2 || a.remote.saved.Index != 0 {
		t.Fatal("timer changed queue")
	}
	a.remote.mu.Unlock()
	select {
	case <-sessions:
		t.Fatal("timer started following song")
	default:
	}
}
func TestRemoteNaturalAdvanceAndDeadline(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	s := awaitSession(t, sessions)
	close(s.finish)
	s = awaitSession(t, sessions)
	remoteWait(t, a.remote, func() bool { return a.remote.saved.Index == 1 })
	remoteOK(t, a, map[string]any{"action": "timer", "deadline": time.Now().Add(-time.Second).UnixMilli()})
	remoteWait(t, a.remote, func() bool { return a.remote.state == "pause" })
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("deadline leaked transport")
	}
}
func TestRemoteConnectionFailureAndPersistence(t *testing.T) {
	a, _ := remoteFixture(t)
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		return nil, errors.New("pairing rejected")
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a"}})
	remoteWait(t, a.remote, func() bool {
		return a.remote.state == "pause" && strings.Contains(a.remote.lastError, "pairing rejected")
	})
	res := request(t, a, "GET", "/api/remote", nil)
	var status map[string]any
	if e := json.Unmarshal(res.Body.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	if status["configured"] != true || status["protocol"] != "airplay2" {
		t.Fatal(status)
	}
	version := status["queueVersion"].(string)
	res = request(t, a, "GET", "/api/remote?queueVersion="+version, nil)
	if strings.Contains(res.Body.String(), `"queue":`) {
		t.Fatal("unchanged queue sent again")
	}
	remoteOK(t, a, map[string]any{"action": "seek", "position": 1234})
	a.remote.commands.Lock()
	if e := a.remote.persist(); e != nil {
		t.Fatal(e)
	}
	a.remote.commands.Unlock()
	b, e := os.ReadFile(filepath.Join(a.config.DataDir, "remote.json"))
	if e != nil {
		t.Fatal(e)
	}
	var saved RemoteSaved
	if e = json.Unmarshal(b, &saved); e != nil || saved.Position != 1234 {
		t.Fatal("resume not persisted", e)
	}
	res = request(t, a, "PUT", "/api/owntone-settings", map[string]string{"ruleSet": ""})
	if res.Code != 404 {
		t.Fatal("removed backend settings still exposed")
	}
}

func TestRemoteHistorySurvivesPauseAndResume(t *testing.T) {
	a, sessions := remoteFixture(t)
	a.remote.connect = func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error) {
		s := &testAirplaySession{progress: 16 * time.Second, done: make(chan struct{}), finish: make(chan struct{}), started: make(chan struct{})}
		sessions <- s
		return s, nil
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a"}})
	awaitSession(t, sessions)
	remoteOK(t, a, map[string]any{"action": "pause"})
	remoteOK(t, a, map[string]any{"action": "play"})
	awaitSession(t, sessions)
	remoteOK(t, a, map[string]any{"action": "pause"})
	if track := a.store.Tracks([]string{"a"})[0]; track.PlayCount != 1 {
		t.Fatalf("resume double-counted song: %d", track.PlayCount)
	}
}
func TestRemoteRejectsCorruptIdentityWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "airplay-identity.json")
	bad := []byte("{broken identity")
	if e := os.WriteFile(path, bad, 0600); e != nil {
		t.Fatal(e)
	}
	a, e := New(Config{DataDir: dir, FFmpeg: "ffmpeg", FFprobe: "ffprobe"})
	if e == nil {
		a.Close()
		t.Fatal("corrupt credentials accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(bad) {
		t.Fatal("corrupt identity silently overwritten")
	}
}

func TestOutputSwitchStopsAndReadsVolume(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a"}, "playing": true})
	old := awaitSession(t, sessions)
	a.remote.mu.Lock()
	a.remote.devices["other"] = airplay.Device{ID: "other"}
	a.remote.mu.Unlock()
	a.remote.readVolume = func(_ context.Context, d airplay.Device, _ string, _ *airplay.Credentials) (int, error) {
		select {
		case <-old.done:
		default:
			t.Error("old output still active during volume query")
		}
		if d.ID != "other" {
			t.Errorf("queried %s", d.ID)
		}
		return 67, nil
	}
	remoteOK(t, a, map[string]any{"action": "outputs", "outputs": []string{"other"}})
	a.remote.mu.Lock()
	if a.remote.state != "stop" || a.remote.saved.Position != 0 || a.remote.saved.Volume != 67 || !a.remote.volumeKnown || a.remote.volumePending {
		t.Error("output did not stop/adopt receiver volume")
	}
	a.remote.mu.Unlock()
	select {
	case <-sessions:
		t.Fatal("switch started playback")
	default:
	}
	remoteOK(t, a, map[string]any{"action": "play"})
	newSession := awaitSession(t, sessions)
	if len(newSession.volumeWrites) != 0 {
		t.Fatal("playback overwrote device volume")
	}
	a.remote.readVolume = func(context.Context, airplay.Device, string, *airplay.Credentials) (int, error) {
		return 0, errors.New("unavailable")
	}
	remoteOK(t, a, map[string]any{"action": "outputs", "outputs": []string{"speaker"}})
	status := request(t, a, "GET", "/api/remote", nil)
	var body struct {
		Player struct {
			State  string `json:"state"`
			Volume *int   `json:"volume"`
		} `json:"player"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Player.State != "stop" || body.Player.Volume != nil {
		t.Fatal("failed volume read leaked old value or resumed playback")
	}
}

func TestUnsupportedDiscoveredOutputCannotBeSelected(t *testing.T) {
	a, _ := remoteFixture(t)
	a.remote.mu.Lock()
	a.remote.devices["legacy"] = airplay.Device{ID: "legacy", UnsupportedReason: "Receiver does not advertise AirPlay 2 audio support"}
	a.remote.mu.Unlock()
	for _, command := range []map[string]any{{"action": "outputs", "outputs": []string{"legacy"}}, {"action": "pair", "outputId": "legacy", "pin": ""}} {
		response := request(t, a, "POST", "/api/remote", command)
		if response.Code != 400 {
			t.Fatalf("accepted unsupported receiver: %d", response.Code)
		}
	}
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if a.remote.saved.Output != "speaker" {
		t.Fatal("invalid selection replaced current output")
	}
}

func TestOutputPairingIsShownOnlyWhenRequired(t *testing.T) {
	a, _ := remoteFixture(t)
	a.remote.discover = func(context.Context) ([]airplay.Device, error) {
		return []airplay.Device{{ID: "open"}, {ID: "pin", RequiresAuth: true}, {ID: "paired", RequiresAuth: true}, {ID: "rejected"}}, nil
	}
	a.remote.mu.Lock()
	a.remote.identity.Credentials["paired"] = airplay.Credentials{}
	a.remote.identity.Credentials["rejected"] = airplay.Credentials{}
	a.remote.pairingRequired["rejected"] = true
	a.remote.mu.Unlock()
	for i := 0; i < 2; i++ {
		response := request(t, a, "GET", "/api/outputs", nil)
		var body struct {
			Outputs []airplay.Device `json:"outputs"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Outputs) != 4 {
			t.Fatal("missing outputs")
		}
		want := map[string]bool{"open": false, "pin": true, "paired": false, "rejected": true}
		for _, device := range body.Outputs {
			if device.RequiresAuth != want[device.ID] {
				t.Fatalf("%s requires_auth=%v", device.ID, device.RequiresAuth)
			}
		}
	}
}
