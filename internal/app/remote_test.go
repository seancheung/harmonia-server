package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestRemoteAutomaticQueueAndManualAppend(t *testing.T) {
	a, _ := remoteFixture(t)
	if err := a.store.Update(func(st *State) error {
		base := st.Tracks[0]
		st.Tracks = nil
		for i := 0; i < 650; i++ {
			track := base
			track.ID = fmt.Sprintf("%03d", i)
			track.Title = track.ID
			st.Tracks = append(st.Tracks, track)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := request(t, a, "GET", browseURL("/api/queue/query", BrowseQuery{Section: "songs", Sort: "title", Start: "500"}), nil)
	var q struct{ IDs []string }
	if err := json.Unmarshal(res.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": q.IDs, "index": 499, "playing": false})
	a.remote.mu.Lock()
	if len(a.remote.saved.Queue) != 500 || a.remote.saved.Queue[0] != "001" || a.remote.saved.Queue[a.remote.saved.Index] != "500" {
		t.Error("wrong AirPlay queue or start index")
	}
	a.remote.mu.Unlock()
	remoteOK(t, a, map[string]any{"action": "append", "ids": []string{"649"}, "position": 500})
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if len(a.remote.saved.Queue) != 501 || a.remote.saved.Queue[500] != "649" || a.remote.saved.Index != 499 {
		t.Error("manual append was capped or moved selection")
	}
}

func TestServerGainIndependentOfPlaybackClients(t *testing.T) {
	a, _ := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "gain", "gain": "auto", "preamp": -3.5, "protect": true})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "index": 1, "position": 1234, "playing": false, "gainContext": "album", "gain": "off", "preamp": 20, "protect": false})
	a.remote.mu.Lock()
	saved := a.remote.saved
	a.remote.mu.Unlock()
	if saved.Gain != "auto" || saved.Preamp != -3.5 || !saved.Protect || remoteGainMode(saved) != "album" {
		t.Fatal("client overwrote server gain", saved)
	}
	remoteOK(t, a, map[string]any{"action": "gain", "gain": "track", "preamp": 2, "protect": false})
	a.remote.mu.Lock()
	updated := a.remote.saved
	a.remote.mu.Unlock()
	if updated.Index != saved.Index || updated.Position != saved.Position || updated.PlaybackSession != saved.PlaybackSession || fmt.Sprint(updated.ShuffleVisited) != fmt.Sprint(saved.ShuffleVisited) {
		t.Fatal("settings changed playback progress")
	}
	restored, err := NewRemote(a)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.saved.Gain != "track" || restored.saved.Preamp != 2 || restored.saved.Protect {
		t.Fatal("server settings not persisted")
	}
	for _, body := range []map[string]any{
		{"action": "gain", "gain": "invalid", "preamp": 0},
		{"action": "gain", "gain": "off", "preamp": 31},
	} {
		if request(t, a, "POST", "/api/remote", body).Code != 400 {
			t.Fatal("invalid gain accepted")
		}
	}
	res := request(t, a, "GET", "/api/remote", nil)
	var status struct {
		ReplayGain struct {
			Gain    string
			Preamp  float64
			Protect bool
		}
		AudioParameters struct{ Gain string }
	}
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ReplayGain.Gain != "track" || status.ReplayGain.Preamp != 2 || status.AudioParameters.Gain != "track" {
		t.Fatal(res.Body.String())
	}
}

func TestServerGainUpdatePreservesActiveRound(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "shuffle", "shuffle": true})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "gainContext": "album"})
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	session := a.remote.saved.PlaybackSession
	a.remote.mu.Unlock()
	remoteOK(t, a, map[string]any{"action": "gain", "gain": "auto", "preamp": -2, "protect": true})
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if a.remote.saved.PlaybackSession != session || !a.remote.saved.Shuffle || len(a.remote.saved.ShuffleVisited) != 1 || a.remote.saved.ShuffleVisited[0] != "a" || remoteGainMode(a.remote.saved) != "album" {
		t.Fatal("gain update reset active round")
	}
}

func TestRemoteFailurePolicySkipsMissingTrackAndPersists(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "failure", "failure": "skip"})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}, "playing": false})
	if err := a.store.Update(func(st *State) error { st.Tracks[0].Path = "missing.wav"; return nil }); err != nil {
		t.Fatal(err)
	}
	remoteOK(t, a, map[string]any{"action": "play"})
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	index := a.remote.saved.Index
	a.remote.mu.Unlock()
	if index != 1 {
		t.Fatal("did not skip missing track")
	}
	remoteOK(t, a, map[string]any{"action": "pause"})
	restored, err := NewRemote(a)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.saved.Failure != "skip" {
		t.Fatal("failure policy not saved")
	}
	if request(t, a, "POST", "/api/remote", map[string]any{"action": "failure", "failure": "bad"}).Code != 400 {
		t.Fatal("invalid policy accepted")
	}
}

func TestRemoteSkipAllFailedIsBounded(t *testing.T) {
	for _, shuffle := range []bool{false, true} {
		for _, repeat := range []string{"off", "single", "all"} {
			r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b", "a", "c"}, Failure: "skip", Repeat: repeat, Shuffle: shuffle}}
			r.resetShuffleLocked()
			attempts := 0
			for {
				attempts++
				if !r.skipFailed(remoteTrackError{errors.New("decode failed")}) {
					break
				}
				if attempts > 3 {
					t.Fatal("retried failed tracks forever")
				}
			}
			if attempts != 3 || r.state != "stop" {
				t.Fatalf("shuffle=%v repeat=%s attempts=%d state=%s", shuffle, repeat, attempts, r.state)
			}
		}
	}
	r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b"}, Failure: "stop"}}
	if r.skipFailed(remoteTrackError{errors.New("failed")}) || r.saved.Index != 0 {
		t.Fatal("stop policy skipped")
	}
}

func TestRemoteSkipAllMissingSourcesStops(t *testing.T) {
	a, _ := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "failure", "failure": "skip"})
	remoteOK(t, a, map[string]any{"action": "repeat", "repeat": "all"})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b", "c"}, "playing": false})
	if err := a.store.Update(func(st *State) error {
		for i := range st.Tracks {
			st.Tracks[i].Path = "missing.wav"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := request(t, a, "POST", "/api/remote", map[string]any{"action": "play"})
	if res.Code != 400 {
		t.Fatal(res.Body.String())
	}
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if len(a.remote.failed) != 3 || a.remote.state != "stop" {
		t.Fatal("all-failed playback did not stop")
	}
}

func TestRemoteSkipDoesNotRetryReceiverErrors(t *testing.T) {
	r := &Remote{saved: RemoteSaved{Queue: []string{"a", "b"}, Failure: "skip", Repeat: "all"}}
	if r.skipFailed(errors.New("receiver disconnected")) || r.saved.Index != 0 {
		t.Fatal("receiver failure must not consume queue")
	}
}

func TestRemoteStartSkipsStaleSource(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "failure", "failure": "skip"})
	if err := a.store.Update(func(st *State) error { st.Tracks[0].Path = "missing.wav"; return nil }); err != nil {
		t.Fatal(err)
	}
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	index := a.remote.saved.Index
	a.remote.mu.Unlock()
	if index != 1 {
		t.Fatal("start did not skip stale source")
	}
}

func TestRemoteDecodeFailureSkipsToNextTrack(t *testing.T) {
	a, sessions := remoteFixture(t)
	root := a.store.Read().Sources[0].Path
	if err := os.WriteFile(filepath.Join(root, "broken.wav"), []byte("invalid audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *State) error { st.Tracks[0].Path = "broken.wav"; return nil }); err != nil {
		t.Fatal(err)
	}
	remoteOK(t, a, map[string]any{"action": "failure", "failure": "skip"})
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	broken := awaitSession(t, sessions)
	close(broken.finish)
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if a.remote.saved.Index != 1 {
		t.Fatal("decode failure was not skipped")
	}
}
