package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

type remoteSession interface {
	Stream(context.Context, io.Reader, func(time.Duration)) error
	Volume(int) error
	Metadata(airplay.TrackMetadata) error
	Artwork([]byte) error
	Close()
}
type RemoteSaved struct {
	PlaybackSession string   `json:"playbackSession,omitempty"`
	Listened        float64  `json:"listened,omitempty"`
	Counted         bool     `json:"counted,omitempty"`
	Seeked          bool     `json:"seeked,omitempty"`
	Queue           []string `json:"queue"`
	Index           int      `json:"index"`
	Position        int      `json:"position"`
	Output          string   `json:"output"`
	Volume          int      `json:"volume"`
	Gain            string   `json:"gain"`
	GainContext     string   `json:"gainContext"`
	Preamp          float64  `json:"preamp"`
	Protect         bool     `json:"protect"`
	Repeat          string   `json:"repeat"`
	Shuffle         bool     `json:"shuffle"`
	Deadline        int64    `json:"deadline"`
	Finish          bool     `json:"finish"`
	Waiting         bool     `json:"waiting"`
}
type remoteIdentity struct {
	ID          string                         `json:"id"`
	Credentials map[string]airplay.Credentials `json:"credentials"`
}
type remoteEnd struct {
	generation uint64
	err        error
}
type Remote struct {
	playback                   sync.Mutex
	commands                   sync.Mutex
	mu                         sync.Mutex
	volumeKnown, volumePending bool
	readVolume                 func(context.Context, airplay.Device, string, *airplay.Credentials) (int, error)
	app                        *App
	saved                      RemoteSaved
	identity                   remoteIdentity
	state, lastError           string
	pairingRequired            map[string]bool
	devices                    map[string]airplay.Device
	discover                   func(context.Context) ([]airplay.Device, error)
	connect                    func(context.Context, airplay.Device, string, *airplay.Credentials) (remoteSession, error)
	cancel                     context.CancelFunc
	session                    remoteSession
	generation                 uint64
	end                        chan remoteEnd
	done                       chan struct{}
	wg                         sync.WaitGroup
	pair                       *airplay.Pairing
	pairID                     string
	pairExpiry                 *time.Timer
	closed                     bool
}

func NewRemote(a *App) (*Remote, error) {
	r := &Remote{app: a, state: "stop", devices: map[string]airplay.Device{}, end: make(chan remoteEnd, 16), done: make(chan struct{}), saved: RemoteSaved{Volume: 50, Repeat: "off", GainContext: "track", Protect: true}, discover: airplay.Discover}
	r.pairingRequired = map[string]bool{}
	r.readVolume = airplay.ReadVolume
	r.connect = func(ctx context.Context, d airplay.Device, id string, c *airplay.Credentials) (remoteSession, error) {
		return airplay.Connect(ctx, d, id, c)
	}
	if b, e := os.ReadFile(filepath.Join(a.config.DataDir, "remote.json")); e == nil {
		if e = json.Unmarshal(b, &r.saved); e != nil {
			return nil, fmt.Errorf("read remote queue: %w", e)
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, fmt.Errorf("read remote queue: %w", e)
	}
	r.saved.Index = max(0, min(r.saved.Index, max(0, len(r.saved.Queue)-1)))
	r.saved.Position = max(0, r.saved.Position)
	r.saved.Volume = max(0, min(100, r.saved.Volume))
	r.saved.Waiting = false
	r.saved.Deadline = 0
	r.saved.Finish = false
	if len(r.saved.Queue) > 0 {
		r.state = "pause"
	}
	p := filepath.Join(a.config.DataDir, "airplay-identity.json")
	if b, e := os.ReadFile(p); e == nil {
		if e = json.Unmarshal(b, &r.identity); e != nil {
			return nil, fmt.Errorf("read AirPlay identity: %w", e)
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, fmt.Errorf("read AirPlay identity: %w", e)
	}
	if r.identity.ID == "" {
		r.identity.ID = strings.ToUpper(airplay.Identity())
	}
	if id, err := hex.DecodeString(r.identity.ID); err != nil || len(id) != 8 {
		return nil, errors.New("invalid saved AirPlay sender identity")
	}
	if r.identity.Credentials == nil {
		r.identity.Credentials = map[string]airplay.Credentials{}
	}
	if e := r.saveIdentity(); e != nil {
		return nil, fmt.Errorf("save AirPlay identity: %w", e)
	}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}
func atomicJSON(path string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if e = os.WriteFile(path+".tmp", b, 0600); e != nil {
		return e
	}
	return os.Rename(path+".tmp", path)
}

// commands serializes persisted state, pairing and playback transitions. mu
// only protects snapshots/progress, and is never held across network I/O.
func (r *Remote) persist() error {
	r.mu.Lock()
	saved := r.saved
	saved.Queue = append([]string{}, saved.Queue...)
	r.mu.Unlock()
	return atomicJSON(filepath.Join(r.app.config.DataDir, "remote.json"), saved)
}
func (r *Remote) saveIdentity() error {
	return atomicJSON(filepath.Join(r.app.config.DataDir, "airplay-identity.json"), r.identity)
}
func (r *Remote) cancelPlayback() {
	r.mu.Lock()
	r.generation++
	cancel, session := r.cancel, r.session
	r.cancel = nil
	r.session = nil
	if len(r.saved.Queue) > 0 {
		r.state = "pause"
	} else {
		r.state = "stop"
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if session != nil {
		session.Close()
	}
}
func (r *Remote) Close() {
	r.commands.Lock()
	if r.closed {
		r.commands.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	r.cancelPlayback()
	if r.pairExpiry != nil {
		r.pairExpiry.Stop()
	}
	if r.pair != nil {
		r.pair.Close()
	}
	_ = r.persist()
	r.commands.Unlock()
	r.wg.Wait()
}
func (r *Remote) loop() {
	defer r.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	persistAt := time.Now()
	for {
		select {
		case <-r.done:
			return
		case event := <-r.end:
			r.commands.Lock()
			r.mu.Lock()
			valid := !r.closed && event.generation == r.generation
			r.mu.Unlock()
			if valid {
				r.cancelPlayback()
				r.mu.Lock()
				if event.err != nil {
					r.lastError = event.err.Error()
					if errors.Is(event.err, airplay.ErrPairingRequired) {
						r.pairingRequired[r.saved.Output] = true
					}
				} else {
					// Honor an expired deadline at EOF even before the timer's next tick.
					if r.saved.Deadline > 0 && time.Now().UnixMilli() >= r.saved.Deadline {
						r.saved.Deadline = 0
						r.saved.Waiting = true
					}
					r.saved.Position = 0
					r.resetHistoryLocked(0)
					if r.saved.Waiting {
						r.saved.Waiting = false
						r.state = "pause"
					} else if r.advanceLocked(false) {
						r.mu.Unlock()
						startErr := r.start()
						r.mu.Lock()
						if startErr != nil {
							r.lastError = startErr.Error()
							r.state = "pause"
						}
					} else {
						r.state = "stop"
					}
				}
				r.mu.Unlock()
				_ = r.persist()
			}
			r.commands.Unlock()
		case <-tick.C:
			r.commands.Lock()
			r.mu.Lock()
			pause := false
			if r.saved.Deadline > 0 && time.Now().UnixMilli() >= r.saved.Deadline {
				r.saved.Deadline = 0
				if r.saved.Finish && (r.state == "play" || r.state == "loading") {
					r.saved.Waiting = true
				} else {
					pause = true
					r.saved.Waiting = false
				}
			}
			r.mu.Unlock()
			if pause {
				r.cancelPlayback()
			}
			if time.Since(persistAt) > 5*time.Second || pause {
				_ = r.persist()
				persistAt = time.Now()
			}
			r.commands.Unlock()
		}
	}
}
func (r *Remote) resetHistoryLocked(position int) {
	r.saved.PlaybackSession = newID()
	r.saved.Listened = 0
	r.saved.Counted = false
	r.saved.Seeked = position > 0
}
func (r *Remote) advanceLocked(manual bool) bool {
	n := len(r.saved.Queue)
	if n == 0 {
		return false
	}
	if !manual && r.saved.Repeat == "single" {
		return true
	}
	if r.saved.Shuffle && n > 1 {
		r.saved.Index = (r.saved.Index + 1 + rand.IntN(n-1)) % n
		return true
	}
	if r.saved.Index+1 < n {
		r.saved.Index++
		return true
	}
	if r.saved.Repeat == "all" {
		r.saved.Index = 0
		return true
	}
	return false
}
func (r *Remote) Outputs(w http.ResponseWriter, req *http.Request) {
	devices, e := r.discover(req.Context())
	if e != nil {
		respond(w, 200, map[string]any{"outputs": []any{}, "error": e.Error()})
		return
	}
	r.mu.Lock()
	for i := range devices {
		d := &devices[i]
		d.Selected = d.ID == r.saved.Output
		r.devices[d.ID] = *d
		if _, ok := r.identity.Credentials[d.ID]; ok {
			d.RequiresAuth = false
		}
		d.RequiresAuth = d.RequiresAuth || r.pairingRequired[d.ID]
	}
	r.mu.Unlock()
	respond(w, 200, map[string]any{"outputs": devices, "protocols": []string{"airplay1", "airplay2"}, "maxSelected": 1})
}
func (r *Remote) Status(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	saved := r.saved
	saved.Queue = append([]string{}, saved.Queue...)
	state, lastError := r.state, r.lastError
	protocol := "airplay2"
	if r.devices[saved.Output].Type == "AirPlay 1" {
		protocol = "airplay1"
	}
	var volume any
	if r.volumeKnown {
		volume = saved.Volume
	}
	r.mu.Unlock()
	data, _ := json.Marshal(saved.Queue)
	version := fmt.Sprintf("%x", sha256.Sum256(data))
	length := float64(0)
	if saved.Index < len(saved.Queue) {
		if tracks := r.app.store.Tracks([]string{saved.Queue[saved.Index]}); len(tracks) > 0 {
			length = tracks[0].Duration * 1000
		}
	}
	result := map[string]any{"configured": true, "protocol": protocol, "player": map[string]any{"state": state, "item_progress_ms": saved.Position, "item_length_ms": length, "repeat": saved.Repeat, "shuffle": saved.Shuffle, "volume": volume}, "error": lastError, "deadline": saved.Deadline, "waiting": saved.Waiting, "finish": saved.Finish, "index": saved.Index, "gainContext": saved.GainContext, "queueVersion": version, "outputId": saved.Output, "audioParameters": map[string]any{"ruleSet": "", "gain": saved.Gain, "preamp": saved.Preamp, "protect": saved.Protect}, "transportFormat": map[string]any{"codec": "alac", "sampleRate": airplay.SampleRate, "bitDepth": 16, "channels": 2}}
	if req.URL.Query().Get("queueVersion") != version {
		result["queue"] = r.app.store.Tracks(saved.Queue)
	}
	respond(w, 200, result)
}
func (r *Remote) start() error {
	r.mu.Lock()
	saved := r.saved
	device, ok := r.devices[saved.Output]
	cred, paired := r.identity.Credentials[saved.Output]
	identity := r.identity.ID
	r.mu.Unlock()
	if saved.Output == "" {
		return errors.New("select one AirPlay output first")
	}
	if !ok {
		return errors.New("selected AirPlay output is unavailable; refresh devices")
	}
	if device.UnsupportedReason != "" {
		return errors.New(device.UnsupportedReason)
	}
	if saved.Index < 0 || saved.Index >= len(saved.Queue) {
		return errors.New("queue is empty")
	}
	track, path, e := trackFromState(r.app.store.Read(), saved.Queue[saved.Index])
	if e != nil {
		return e
	}
	if track.Duration > 0 && float64(saved.Position) >= track.Duration*1000 {
		return errors.New("seek position is beyond track duration")
	}
	if device.RequiresAuth && !paired {
		return errors.New("AirPlay output requires pairing; start pairing and enter its PIN")
	}
	r.cancelPlayback()
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.saved.PlaybackSession == "" {
		r.resetHistoryLocked(saved.Position)
	}
	saved.PlaybackSession = r.saved.PlaybackSession
	r.cancel = cancel
	r.state = "loading"
	r.lastError = ""
	generation := r.generation
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.playback.Lock()
		defer r.playback.Unlock()
		if ctx.Err() != nil {
			return
		}
		var listened time.Duration
		playID := saved.PlaybackSession
		counted := saved.Counted
		play := func() error {
			var credentials *airplay.Credentials
			if paired {
				credentials = &cred
			}
			session, e := r.connect(ctx, device, identity, credentials)
			if e != nil {
				return e
			}
			defer session.Close()
			stop := context.AfterFunc(ctx, session.Close)
			defer stop()
			r.mu.Lock()
			if generation != r.generation {
				r.mu.Unlock()
				return context.Canceled
			}
			r.session = session
			volume := r.saved.Volume
			setVolume := r.volumePending
			r.mu.Unlock()
			artwork, artworkErr := remoteArtwork(track.Cover)
			if artworkErr != nil {
				log.Printf("AirPlay artwork for track %s: %v", track.ID, artworkErr)
			}
			if e = session.Metadata(airplay.TrackMetadata{HasArtwork: len(artwork) > 0, ID: track.ID, Title: track.Title, Artist: track.Artist, Album: track.Album, AlbumArtist: track.AlbumArtist, Number: track.Number, Disc: track.Disc, Duration: time.Duration(track.Duration * float64(time.Second)), Position: time.Duration(saved.Position) * time.Millisecond}); e != nil {
				return e
			}
			if artworkErr = session.Artwork(artwork); artworkErr != nil {
				log.Printf("AirPlay artwork for track %s was not accepted: %v", track.ID, artworkErr)
			}
			if setVolume {
				if e = session.Volume(volume); e != nil {
					return e
				}
				r.mu.Lock()
				r.volumePending = false
				r.mu.Unlock()
			}
			gain := ReplayGain(track, saved.Gain, math.Max(-30, math.Min(30, saved.Preamp)), saved.Protect)
			args := []string{"-nostdin", "-v", "error", "-threads", "1", "-ss", fmt.Sprintf("%.3f", float64(saved.Position)/1000), "-i", path, "-map", "0:a:0", "-vn", "-af", fmt.Sprintf("volume=%.9f", gain), "-ar", "44100", "-ac", "2", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"}
			cmd := exec.CommandContext(ctx, r.app.config.FFmpeg, args...)
			stdout, e := cmd.StdoutPipe()
			if e != nil {
				return e
			}
			var stderr limitedLog
			cmd.Stderr = &stderr
			if e = cmd.Start(); e != nil {
				return e
			}
			e = session.Stream(ctx, stdout, func(position time.Duration) {
				listened = position
				r.mu.Lock()
				if generation == r.generation {
					r.state = "play"
					r.saved.Position = saved.Position + int(position.Milliseconds())
					r.saved.Listened = saved.Listened + position.Seconds()
				}
				r.mu.Unlock()
				if !counted && saved.Listened+position.Seconds() >= 15 {
					counted = r.app.store.RecordPlayed(track.ID, playID, saved.Listened+position.Seconds(), false, saved.Seeked, time.Now()) == nil
					r.mu.Lock()
					if generation == r.generation {
						r.saved.Counted = counted
					}
					r.mu.Unlock()
				}
			})
			if e != nil {
				_ = cmd.Process.Kill()
			}
			waitErr := cmd.Wait()
			if e != nil {
				return e
			}
			if waitErr != nil {
				return fmt.Errorf("audio decode: %w: %s", waitErr, stderr.String())
			}
			return nil
		}
		err := play()
		if err == nil && !counted {
			_ = r.app.store.RecordPlayed(track.ID, playID, saved.Listened+listened.Seconds(), true, saved.Seeked, time.Now())
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case r.end <- remoteEnd{generation, err}:
		case <-r.done:
		}
	}()
	return nil
}

type limitedLog struct{ b []byte }

func (l *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	space := 4096 - len(l.b)
	l.b = append(l.b, p[:min(space, n)]...)
	return n, nil
}
func (l *limitedLog) String() string { return string(l.b) }

type remoteCommand struct {
	Action      string   `json:"action"`
	Playing     *bool    `json:"playing"`
	IDs         []string `json:"ids"`
	Outputs     []string `json:"outputs"`
	Index       int      `json:"index"`
	Position    int      `json:"position"`
	Volume      int      `json:"volume"`
	RuleSet     string   `json:"ruleSet"`
	Gain        string   `json:"gain"`
	GainContext string   `json:"gainContext"`
	Preamp      float64  `json:"preamp"`
	Protect     bool     `json:"protect"`
	Deadline    int64    `json:"deadline"`
	Finish      bool     `json:"finish"`
	Repeat      string   `json:"repeat"`
	Shuffle     bool     `json:"shuffle"`
	PIN         string   `json:"pin"`
	OutputID    string   `json:"outputId"`
}

func (r *Remote) Command(w http.ResponseWriter, req *http.Request) {
	var b remoteCommand
	if !decode(w, req, &b) {
		return
	}
	r.commands.Lock()
	defer r.commands.Unlock()
	if r.closed {
		respond(w, 503, map[string]string{"error": "server is shutting down"})
		return
	}
	e := r.command(b)
	if e == nil {
		e = r.persist()
	}
	if e != nil {
		r.mu.Lock()
		r.lastError = e.Error()
		r.mu.Unlock()
		_ = r.persist()
		respond(w, 400, map[string]string{"error": e.Error()})
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (r *Remote) command(b remoteCommand) error {
	r.mu.Lock()
	saved := r.saved
	playing := r.state == "play" || r.state == "loading"
	r.mu.Unlock()
	validate := func(ids []string) error {
		if len(ids) > 10000 {
			return errors.New("queue exceeds 10000 tracks")
		}
		st := r.app.store.Read()
		for _, id := range ids {
			if _, _, e := trackFromState(st, id); e != nil {
				return e
			}
		}
		return nil
	}
	switch b.Action {
	case "outputs":
		if len(b.Outputs) > 1 {
			return errors.New("only one AirPlay output can be selected")
		}
		id := ""
		if len(b.Outputs) == 1 {
			id = b.Outputs[0]
			r.mu.Lock()
			device, ok := r.devices[id]
			r.mu.Unlock()
			if !ok {
				return errors.New("output not found; refresh devices")
			}
			if device.UnsupportedReason != "" {
				return errors.New(device.UnsupportedReason)
			}
		}
		{
			r.cancelPlayback()
			r.mu.Lock()
			r.saved.Output = id
			r.saved.Position = 0
			r.saved.Deadline = 0
			r.saved.Waiting = false
			r.saved.Finish = false
			r.resetHistoryLocked(0)
			r.state = "stop"
			r.lastError = ""
			r.volumeKnown = false
			r.volumePending = false
			device := r.devices[id]
			identity := r.identity.ID
			cred, paired := r.identity.Credentials[id]
			r.mu.Unlock()
			if id != "" {
				var credentials *airplay.Credentials
				if paired {
					credentials = &cred
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				volume, err := r.readVolume(ctx, device, identity, credentials)
				cancel()
				r.mu.Lock()
				if err == nil {
					r.saved.Volume = volume
					r.volumeKnown = true
				} else {
					r.lastError = "Could not read output volume: " + err.Error()
					if errors.Is(err, airplay.ErrPairingRequired) {
						r.pairingRequired[id] = true
					}
				}
				r.mu.Unlock()
			}
		}
	case "pair":
		return r.pairDevice(b)
	case "start":
		if len(b.IDs) == 0 || b.Index < 0 || b.Index >= len(b.IDs) || b.Position < 0 {
			return errors.New("invalid queue or position")
		}
		if e := validate(b.IDs); e != nil {
			return e
		}
		if b.Gain != "" && b.Gain != "off" && b.Gain != "track" && b.Gain != "album" {
			return errors.New("invalid gain mode")
		}
		r.cancelPlayback()
		r.mu.Lock()
		r.saved.Queue = append([]string{}, b.IDs...)
		r.saved.Index = b.Index
		r.saved.Position = b.Position
		r.resetHistoryLocked(b.Position)
		r.saved.Gain = b.Gain
		r.saved.GainContext = "track"
		if b.GainContext == "album" {
			r.saved.GainContext = "album"
		}
		r.saved.Preamp = max(-30, min(30, b.Preamp))
		r.saved.Protect = b.Protect
		r.saved.Waiting = false
		r.state = "pause"
		r.lastError = ""
		r.mu.Unlock()
		if b.Playing == nil || *b.Playing {
			return r.start()
		}
	case "play":
		return r.start()
	case "pause", "local":
		r.cancelPlayback()
		if b.Action == "local" {
			r.mu.Lock()
			r.saved.Deadline = 0
			r.saved.Waiting = false
			r.saved.Finish = false
			r.mu.Unlock()
		}
	case "stop":
		r.cancelPlayback()
		r.mu.Lock()
		r.state = "stop"
		r.saved.Position = 0
		r.saved.Deadline = 0
		r.saved.Finish = false
		r.saved.Waiting = false
		r.resetHistoryLocked(0)
		r.mu.Unlock()
	case "clear":
		r.cancelPlayback()
		r.mu.Lock()
		r.saved.Queue = []string{}
		r.saved.Index = 0
		r.saved.Position = 0
		r.saved.Deadline = 0
		r.saved.Finish = false
		r.saved.Waiting = false
		r.state = "stop"
		r.lastError = ""
		r.mu.Unlock()
	case "seek":
		if b.Position < 0 {
			return errors.New("invalid seek position")
		}
		if saved.Index >= len(saved.Queue) {
			return errors.New("queue is empty")
		}
		tracks := r.app.store.Tracks([]string{saved.Queue[saved.Index]})
		if len(tracks) > 0 && tracks[0].Duration > 0 && float64(b.Position) >= tracks[0].Duration*1000 {
			return errors.New("seek position is beyond track duration")
		}
		r.cancelPlayback()
		r.mu.Lock()
		r.saved.Position = b.Position
		r.saved.Seeked = true
		r.mu.Unlock()
		if playing {
			return r.start()
		}
	case "select", "next", "previous":
		index := b.Index
		if b.Action == "previous" {
			index = max(0, saved.Index-1)
		}
		if b.Action == "next" {
			r.mu.Lock()
			ok := r.advanceLocked(true)
			index = r.saved.Index
			r.mu.Unlock()
			if !ok {
				r.cancelPlayback()
				return nil
			}
		}
		if index < 0 || index >= len(saved.Queue) {
			return errors.New("invalid queue index")
		}
		r.cancelPlayback()
		r.mu.Lock()
		r.saved.Index = index
		r.resetHistoryLocked(0)
		r.saved.Position = 0
		r.saved.Waiting = false
		r.mu.Unlock()
		return r.start()
	case "append":
		if len(saved.Queue)+len(b.IDs) > 10000 {
			return errors.New("queue exceeds 10000 tracks")
		}
		if e := validate(b.IDs); e != nil {
			return e
		}
		position := b.Position
		if position < 0 || position > len(saved.Queue) {
			position = len(saved.Queue)
		}
		r.mu.Lock()
		q := append([]string{}, r.saved.Queue[:position]...)
		q = append(q, b.IDs...)
		q = append(q, r.saved.Queue[position:]...)
		r.saved.Queue = q
		if len(saved.Queue) > 0 && position <= r.saved.Index {
			r.saved.Index += len(b.IDs)
		}
		r.mu.Unlock()
	case "move":
		if b.Index < 0 || b.Index >= len(saved.Queue) || b.Position < 0 || b.Position >= len(saved.Queue) {
			return errors.New("invalid queue position")
		}
		r.mu.Lock()
		q := r.saved.Queue
		id := q[b.Index]
		q = append(q[:b.Index], q[b.Index+1:]...)
		q = append(q, "")
		copy(q[b.Position+1:], q[b.Position:])
		q[b.Position] = id
		r.saved.Queue = q
		if r.saved.Index == b.Index {
			r.saved.Index = b.Position
		} else if b.Index < r.saved.Index && b.Position >= r.saved.Index {
			r.saved.Index--
		} else if b.Index > r.saved.Index && b.Position <= r.saved.Index {
			r.saved.Index++
		}
		r.mu.Unlock()
	case "remove":
		if b.Index < 0 || b.Index >= len(saved.Queue) {
			return errors.New("invalid queue index")
		}
		current := b.Index == saved.Index
		if current {
			r.cancelPlayback()
		}
		r.mu.Lock()
		r.saved.Queue = append(r.saved.Queue[:b.Index], r.saved.Queue[b.Index+1:]...)
		if b.Index < r.saved.Index {
			r.saved.Index--
		}
		if current {
			r.saved.Index = min(r.saved.Index, max(0, len(r.saved.Queue)-1))
			r.resetHistoryLocked(0)
			r.saved.Position = 0
			r.saved.Waiting = false
		}
		empty := len(r.saved.Queue) == 0
		if empty {
			r.state = "stop"
		}
		r.mu.Unlock()
		if current && playing && !empty {
			return r.start()
		}
	case "volume":
		if b.Volume < 0 || b.Volume > 100 {
			return errors.New("volume must be between 0 and 100")
		}
		r.mu.Lock()
		session := r.session
		r.mu.Unlock()
		if session != nil {
			if e := session.Volume(b.Volume); e != nil {
				return e
			}
		}
		r.mu.Lock()
		r.saved.Volume = b.Volume
		r.volumeKnown = true
		r.volumePending = session == nil
		r.mu.Unlock()
	case "repeat":
		if b.Repeat != "off" && b.Repeat != "all" && b.Repeat != "single" {
			return errors.New("invalid repeat mode")
		}
		r.mu.Lock()
		r.saved.Repeat = b.Repeat
		r.mu.Unlock()
	case "shuffle":
		r.mu.Lock()
		r.saved.Shuffle = b.Shuffle
		r.mu.Unlock()
	case "timer":
		if b.Deadline < 0 {
			return errors.New("invalid timer deadline")
		}
		r.mu.Lock()
		r.saved.Deadline = b.Deadline
		r.saved.Finish = b.Finish
		r.saved.Waiting = b.Finish && b.Deadline == 0
		r.mu.Unlock()
	default:
		return errors.New("unknown remote command")
	}
	return nil
}
func (r *Remote) pairDevice(b remoteCommand) error {
	if b.PIN == "" {
		r.mu.Lock()
		device, ok := r.devices[b.OutputID]
		id := r.identity.ID
		r.mu.Unlock()
		if !ok {
			return errors.New("output not found; refresh devices")
		}
		if device.UnsupportedReason != "" {
			return errors.New(device.UnsupportedReason)
		}
		if r.pair != nil {
			r.pair.Close()
			r.pair = nil
		}
		if r.pairExpiry != nil {
			r.pairExpiry.Stop()
		}
		if device.Type == "AirPlay 1" {
			return errors.New("AirPlay 1 PIN pairing is not supported")
		}
		p, e := airplay.BeginPair(device, id)
		if e != nil {
			return e
		}
		r.pair = p
		r.pairID = b.OutputID
		r.pairExpiry = time.AfterFunc(2*time.Minute, func() {
			r.commands.Lock()
			defer r.commands.Unlock()
			if r.pair == p {
				p.Close()
				r.pair = nil
			}
		})
		return nil
	}
	if r.pair == nil || r.pairID != b.OutputID {
		return errors.New("start pairing with an empty PIN first")
	}
	p := r.pair
	r.pair = nil
	r.pairExpiry.Stop()
	cred, e := p.Finish(b.PIN)
	if e != nil {
		return e
	}
	r.mu.Lock()
	r.identity.Credentials[b.OutputID] = cred
	delete(r.pairingRequired, b.OutputID)
	r.mu.Unlock()
	return r.saveIdentity()
}
