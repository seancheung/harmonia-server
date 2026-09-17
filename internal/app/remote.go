package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Remote struct {
	commands       sync.Mutex
	controls       sync.Mutex
	pending        *RemoteSaved
	cancelQueue    bool
	holdPlayback   bool
	repeatCommands sync.Mutex
	persistMu      sync.Mutex
	mu             sync.Mutex
	app            *App
	client         *http.Client
	done           chan struct{}
	wg             sync.WaitGroup
	deadline       int64
	finish         bool
	waiting        bool
	item           float64
	last           map[string]any
	lastError      string
	saved          RemoteSaved
	statsItem      float64
	statsSeconds   float64
	statsSeeked    bool
	statsCounted   bool
	statsPrevious  float64
	statsAt        time.Time
}

type RemoteSaved struct {
	Queue          []string  `json:"queue"`
	ItemIDs        []float64 `json:"itemIds"`
	Index          int       `json:"index"`
	Deadline       int64     `json:"deadline"`
	Finish         bool      `json:"finish"`
	Waiting        bool      `json:"waiting"`
	Trimmed        bool      `json:"trimmed"`
	RuleSet        string    `json:"ruleSet"`
	Gain           string    `json:"gain"`
	GainContext    string    `json:"gainContext"`
	Preamp         float64   `json:"preamp"`
	Protect        bool      `json:"protect"`
	Repeat         string    `json:"repeat"`
	ResumePosition *int      `json:"resumePosition,omitempty"`
}

func NewRemote(a *App) *Remote {
	r := &Remote{app: a, client: &http.Client{Timeout: 10 * time.Second}, done: make(chan struct{}), last: map[string]any{}}
	if data, e := os.ReadFile(filepath.Join(a.config.DataDir, "remote.json")); e == nil {
		_ = json.Unmarshal(data, &r.saved)
		r.deadline = r.saved.Deadline
		r.finish = r.saved.Finish
		r.waiting = r.saved.Waiting
		if r.saved.Index < len(r.saved.ItemIDs) {
			r.item = r.saved.ItemIDs[r.saved.Index]
		}
	}
	r.wg.Add(1)
	go r.loop()
	return r
}
func (r *Remote) Close() { close(r.done); r.wg.Wait() }
func (r *Remote) call(method, path string, body any) (map[string]any, error) {
	if r.app.config.OwnTone == "" {
		return nil, errors.New("AirPlay requires HARMONIA_OWNTONE; configure an OwnTone server on the speaker network")
	}
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, e := http.NewRequest(method, strings.TrimRight(r.app.config.OwnTone, "/")+"/api/"+path, bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	client := r.client
	adding := method == "POST" && strings.HasPrefix(path, "queue/items/add?")
	if adding {
		// OwnTone probes each URL, which can require a cold full-track transcode.
		copy := *client
		copy.Timeout = 2 * time.Minute
		client = &copy
	}
	res, e := client.Do(req)
	if e != nil {
		if adding {
			return nil, &queueOutcomeUnknown{e}
		}
		return nil, e
	}
	defer res.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024))
	if readErr != nil {
		if adding {
			return nil, &queueOutcomeUnknown{readErr}
		}
		return nil, readErr
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("OwnTone: HTTP %d %s", res.StatusCode, string(b))
	}
	out := map[string]any{}
	if len(b) > 0 {
		if e = json.Unmarshal(b, &out); e != nil {
			if adding {
				return nil, &queueOutcomeUnknown{e}
			}
			return nil, e
		}
	}
	return out, nil
}
func (r *Remote) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			if r.app.config.OwnTone == "" {
				continue
			}
			if !r.commands.TryLock() {
				// Reads must continue while a queue write waits for media probing.
				if state, err := r.call("GET", "player", nil); err == nil {
					r.mu.Lock()
					r.last = state
					if r.pending != nil && r.pending.ResumePosition == nil {
						for i, id := range r.pending.ItemIDs {
							if id == state["item_id"] {
								r.pending.Index = i
								break
							}
						}
					}
					r.mu.Unlock()
				}
				continue
			}
			state, e := r.call("GET", "player", nil)
			r.mu.Lock()
			if e != nil {
				r.lastError = e.Error()
				r.mu.Unlock()
				r.commands.Unlock()
				continue
			}
			r.lastError = ""
			r.account(state)
			r.last = state
			item, _ := state["item_id"].(float64)
			playing := state["state"] == "play"
			stop := false
			arm := false
			for i, id := range r.saved.ItemIDs {
				if id == item && r.saved.ResumePosition == nil {
					r.saved.Index = i
					break
				}
			}
			if r.deadline > 0 && time.Now().UnixMilli() >= r.deadline {
				r.deadline = 0
				if r.finish && playing {
					r.waiting = true
					r.item = item
					arm = true
				} else {
					stop = playing
					r.waiting = false
				}
			}
			if r.waiting {
				if item != r.item || state["state"] == "stop" {
					stop = playing
					r.waiting = false
				}
			}
			r.mu.Unlock()
			if arm {
				if e := r.armEnd(); e != nil {
					r.mu.Lock()
					r.lastError = e.Error()
					r.mu.Unlock()
				}
			}
			if stop {
				_, _ = r.call("PUT", "player/pause", nil)
			}
			r.commands.Unlock()
		}
	}
}
func (r *Remote) Outputs(w http.ResponseWriter, req *http.Request) {
	out, e := r.call("GET", "outputs", nil)
	if e != nil {
		respond(w, 200, map[string]any{"outputs": []any{}, "error": e.Error()})
		return
	}
	respond(w, 200, out)
}
func (r *Remote) Status(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	saved := r.saved
	if r.pending != nil {
		saved = *r.pending
	}
	data, _ := json.Marshal(saved.Queue)
	version := fmt.Sprintf("%x", sha256.Sum256(data))
	player := make(map[string]any, len(r.last))
	for key, value := range r.last {
		player[key] = value
	}
	if saved.ResumePosition != nil {
		player["state"] = "pause"
		player["item_progress_ms"] = *saved.ResumePosition
	}
	result := map[string]any{"configured": r.app.config.OwnTone != "", "player": player, "error": r.lastError, "deadline": r.deadline, "waiting": r.waiting, "finish": r.finish, "index": saved.Index, "gainContext": saved.GainContext, "queueVersion": version}
	if req.URL.Query().Get("queueVersion") != version {
		tracks := []Track{}
		byID := map[string]Track{}
		for _, t := range r.app.store.Read().Tracks {
			byID[t.ID] = t
		}
		for _, id := range saved.Queue {
			if t, ok := byID[id]; ok {
				tracks = append(tracks, t)
			} else {
				tracks = append(tracks, Track{ID: id, Missing: true})
			}
		}
		result["queue"] = tracks
	}
	respond(w, 200, result)
}
func (r *Remote) Command(w http.ResponseWriter, req *http.Request) {
	var b struct {
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
	if !decode(w, req, &b) {
		return
	}
	r.mu.Lock()
	loadingQueue := r.pending != nil
	r.mu.Unlock()
	if b.Action == "pause" || b.Action == "local" || b.Action == "clear" || (b.Action == "play" && loadingQueue) {
		r.controlDuringQueue(w, b.Action)
		return
	}
	// Repeat does not mutate queue membership and must remain available while
	// OwnTone probes stream URLs during a long queue submission.
	if b.Action == "repeat" {
		r.repeatCommands.Lock()
		defer r.repeatCommands.Unlock()
		if b.Repeat != "off" && b.Repeat != "all" && b.Repeat != "single" {
			respond(w, 502, map[string]string{"error": "invalid repeat mode"})
			return
		}
		if _, err := r.call("PUT", "player/repeat?state="+b.Repeat, nil); err != nil {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
		r.mu.Lock()
		r.saved.Repeat = b.Repeat
		if r.last == nil {
			r.last = map[string]any{}
		}
		r.last["repeat"] = b.Repeat
		r.mu.Unlock()
		r.persist()
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	r.commands.Lock()
	defer r.commands.Unlock()
	if b.Action == "append" {
		r.mu.Lock()
		r.cancelQueue = false
		r.mu.Unlock()
	}
	var e error
	if b.Action == "play" || b.Action == "select" || b.Action == "next" || b.Action == "previous" || b.Action == "append" || b.Action == "move" || b.Action == "remove" {
		if err := r.restoreQueue(); err != nil {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
	}
	switch b.Action {
	case "outputs":
		_, e = r.call("PUT", "outputs/set", map[string]any{"outputs": b.Outputs})
	case "pair":
		_, e = r.call("PUT", "outputs/"+url.PathEscape(b.OutputID), map[string]any{"pin": b.PIN})
	case "start":
		if len(b.IDs) == 0 || b.Index < 0 || b.Index >= len(b.IDs) {
			e = errors.New("queue is empty")
			break
		}
		uris := []string{}
		library := r.app.store.Read()
		for _, id := range b.IDs {
			if _, _, err := trackFromState(library, id); err != nil {
				e = err
				break
			}
			q := url.Values{"output": {"airplay"}, "ruleSet": {b.RuleSet}, "gain": {b.Gain}, "preamp": {fmt.Sprint(b.Preamp)}, "protect": {fmt.Sprint(b.Protect)}, "token": {r.app.config.Token}}
			uris = append(uris, strings.TrimRight(r.app.config.PublicURL, "/")+"/api/tracks/"+id+"/stream?"+q.Encode())
		}
		if e != nil {
			break
		}
		r.mu.Lock()
		r.cancelQueue, r.holdPlayback = false, b.Playing != nil && !*b.Playing
		r.pending = &RemoteSaved{Queue: append([]string{}, b.IDs...), Index: b.Index, GainContext: b.GainContext, ResumePosition: &b.Position}
		r.mu.Unlock()
		e = r.replaceURLs(uris, b.Index, b.Position)
		r.controls.Lock()
		r.mu.Lock()
		cancelled := r.cancelQueue
		if cancelled && e == nil {
			e = errQueueCancelled
		}
		resumePosition := r.pending.ResumePosition
		r.pending = nil
		r.mu.Unlock()
		if e == nil {
			r.mu.Lock()
			r.saved.Queue = append([]string{}, b.IDs...)
			r.saved.Index = b.Index
			r.saved.ResumePosition = resumePosition
			r.saved.RuleSet = b.RuleSet
			r.saved.Gain = b.Gain
			r.saved.GainContext = "track"
			if b.GainContext == "album" {
				r.saved.GainContext = "album"
			}
			r.saved.Preamp = b.Preamp
			r.saved.Protect = b.Protect
			r.saved.Trimmed = false
			r.waiting = false
			r.statsSeeked = false
			r.mu.Unlock()
			e = r.mapQueue()
		}
		r.controls.Unlock()
		if errors.Is(e, errQueueCancelled) {
			respond(w, 200, map[string]bool{"ok": true, "cancelled": true})
			return
		}
	case "select":
		r.mu.Lock()
		ids := append([]float64{}, r.saved.ItemIDs...)
		r.waiting = false
		r.statsSeeked = true
		r.mu.Unlock()
		if b.Index < 0 || b.Index >= len(ids) {
			e = errors.New("invalid queue index")
			break
		}
		_, e = r.call("PUT", fmt.Sprintf("player/play?item_id=%.0f", ids[b.Index]), nil)
		if e == nil {
			r.mu.Lock()
			r.saved.ResumePosition = nil
			r.mu.Unlock()
		}
	case "append":
		r.mu.Lock()
		position := len(r.saved.Queue)
		if b.Position >= 0 && b.Position <= position {
			position = b.Position
		}
		r.mu.Unlock()
		e = r.addURLs(b.IDs, position)
		if e == nil {
			r.mu.Lock()
			queue := append([]string{}, r.saved.Queue[:position]...)
			queue = append(queue, b.IDs...)
			queue = append(queue, r.saved.Queue[position:]...)
			r.saved.Queue = queue
			r.mu.Unlock()
			e = r.mapQueue()
		}
	case "move":
		r.mu.Lock()
		ids := append([]float64{}, r.saved.ItemIDs...)
		r.mu.Unlock()
		if b.Index < 0 || b.Index >= len(ids) || b.Position < 0 || b.Position >= len(ids) {
			e = errors.New("invalid queue position")
			break
		}
		_, e = r.call("PUT", fmt.Sprintf("queue/items/%.0f?new_position=%d", ids[b.Index], b.Position), nil)
		if e == nil {
			r.mu.Lock()
			queue := r.saved.Queue
			id := queue[b.Index]
			queue = append(queue[:b.Index], queue[b.Index+1:]...)
			queue = append(queue, "")
			copy(queue[b.Position+1:], queue[b.Position:])
			queue[b.Position] = id
			r.saved.Queue = queue
			r.mu.Unlock()
			e = r.mapQueue()
		}
	case "remove":
		r.mu.Lock()
		ids := append([]float64{}, r.saved.ItemIDs...)
		r.mu.Unlock()
		if b.Index < 0 || b.Index >= len(ids) {
			e = errors.New("invalid queue index")
			break
		}
		_, e = r.call("DELETE", fmt.Sprintf("queue/items/%.0f", ids[b.Index]), nil)
		if e == nil {
			r.mu.Lock()
			r.saved.Queue = append(r.saved.Queue[:b.Index], r.saved.Queue[b.Index+1:]...)
			r.mu.Unlock()
			e = r.mapQueue()
		}
	case "clear":
		_, e = r.call("PUT", "queue/clear", nil)
		if e == nil {
			r.mu.Lock()
			r.saved.Queue = []string{}
			r.saved.ItemIDs = []float64{}
			r.saved.Index = 0
			r.mu.Unlock()
		}
	case "local", "pause", "play", "next", "previous":
		action := b.Action
		if action == "local" {
			e = r.pauseForLocal()
		} else if action == "play" {
			e = r.resumePlayback()
		} else {
			_, e = r.call("PUT", "player/"+action, nil)
		}
		if b.Action == "local" && e == nil {
			r.mu.Lock()
			r.deadline = 0
			r.waiting = false
			r.finish = false
			r.mu.Unlock()
		}
		if action == "next" || action == "previous" {
			r.mu.Lock()
			r.saved.ResumePosition = nil
			r.waiting = false
			r.mu.Unlock()
		}
	case "seek":
		r.mu.Lock()
		r.statsSeeked = true
		r.statsAt = time.Time{}
		if r.saved.ResumePosition != nil {
			r.saved.ResumePosition = &b.Position
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
		_, e = r.call("PUT", fmt.Sprintf("player/seek?position_ms=%d", b.Position), nil)
	case "volume":
		_, e = r.call("PUT", fmt.Sprintf("player/volume?volume=%d", max(0, min(100, b.Volume))), nil)
	case "shuffle":
		_, e = r.call("PUT", fmt.Sprintf("player/shuffle?state=%t", b.Shuffle), nil)
	case "timer":
		r.mu.Lock()
		r.deadline = b.Deadline
		r.finish = b.Finish
		r.waiting = b.Finish && b.Deadline == 0
		r.item, _ = r.last["item_id"].(float64)
		r.mu.Unlock()
		if b.Finish && b.Deadline == 0 {
			e = r.armEnd()
		} else {
			e = r.restoreQueue()
		}
	default:
		e = errors.New("unknown remote command")
	}
	if e != nil {
		respond(w, 502, map[string]string{"error": e.Error()})
		return
	}
	if state, err := r.call("GET", "player", nil); err == nil {
		r.mu.Lock()
		r.last = state
		item, _ := state["item_id"].(float64)
		for i, id := range r.saved.ItemIDs {
			if id == item && r.saved.ResumePosition == nil {
				r.saved.Index = i
				break
			}
		}
		r.mu.Unlock()
	}
	r.persist()
	respond(w, 200, map[string]bool{"ok": true})
}

func (r *Remote) persist() {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	r.saved.Deadline = r.deadline
	r.saved.Finish = r.finish
	r.saved.Waiting = r.waiting
	data, _ := json.Marshal(r.saved)
	r.mu.Unlock()
	p := filepath.Join(r.app.config.DataDir, "remote.json")
	if e := os.WriteFile(p+".tmp", data, 0600); e == nil {
		_ = os.Rename(p+".tmp", p)
	}
}
func (r *Remote) mapQueue() error {
	items, e := r.queueItems()
	if e != nil {
		return e
	}
	ids := []float64{}
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			if id, ok := m["id"].(float64); ok {
				ids = append(ids, id)
			}
		}
	}
	r.mu.Lock()
	if r.pending != nil {
		r.pending.ItemIDs = ids
	} else {
		r.saved.ItemIDs = ids
	}
	r.mu.Unlock()
	return nil
}
func (r *Remote) addURLs(ids []string, position int) error {
	r.mu.Lock()
	saved := r.saved
	r.mu.Unlock()
	uris := []string{}
	library := r.app.store.Read()
	for _, id := range ids {
		if _, _, e := trackFromState(library, id); e != nil {
			return e
		}
		q := url.Values{"output": {"airplay"}, "ruleSet": {saved.RuleSet}, "gain": {saved.Gain}, "preamp": {fmt.Sprint(saved.Preamp)}, "protect": {fmt.Sprint(saved.Protect)}, "token": {r.app.config.Token}}
		uris = append(uris, strings.TrimRight(r.app.config.PublicURL, "/")+"/api/tracks/"+id+"/stream?"+q.Encode())
	}
	if len(uris) == 0 {
		return nil
	}
	return r.addBatches(uris, position)
}

// Temporarily leave only the current item in OwnTone so its natural completion
// stops playback without cutting audio or briefly starting the following song.
// The complete Harmonia queue remains persisted and is restored on cancellation.
func (r *Remote) armEnd() error {
	r.mu.Lock()
	if r.saved.Trimmed {
		r.mu.Unlock()
		return nil
	}
	ids := append([]float64{}, r.saved.ItemIDs...)
	current := r.item
	r.saved.Trimmed = true
	r.mu.Unlock()
	if _, e := r.call("PUT", "player/repeat?state=off", nil); e != nil {
		return e
	}
	for _, id := range ids {
		if id != current {
			if _, e := r.call("DELETE", fmt.Sprintf("queue/items/%.0f", id), nil); e != nil {
				return e
			}
		}
	}
	r.persist()
	return nil
}
func (r *Remote) restoreQueue() error {
	r.mu.Lock()
	if !r.saved.Trimmed {
		r.mu.Unlock()
		return nil
	}
	queue := append([]string{}, r.saved.Queue...)
	index := r.saved.Index
	repeat := r.saved.Repeat
	r.mu.Unlock()
	if index < 0 || index >= len(queue) {
		return nil
	}
	if e := r.addURLs(queue[:index], 0); e != nil {
		return e
	}
	if e := r.addURLs(queue[index+1:], index+1); e != nil {
		return e
	}
	if repeat == "" {
		repeat = "off"
	}
	if _, e := r.call("PUT", "player/repeat?state="+repeat, nil); e != nil {
		return e
	}
	r.mu.Lock()
	r.saved.Trimmed = false
	r.mu.Unlock()
	return r.mapQueue()
}

// OwnTone progress deltas exclude pauses, buffering and seeks. A new item (or a
// wrap to its beginning) starts a fresh accounting session on the server.
func (r *Remote) account(state map[string]any) {
	item, _ := state["item_id"].(float64)
	progress, _ := state["item_progress_ms"].(float64)
	now := time.Now()
	elapsed := now.Sub(r.statsAt).Seconds()
	length, _ := r.last["item_length_ms"].(float64)
	remaining := (length - r.statsPrevious) / 1000
	ended := item != r.statsItem || progress+500 < r.statsPrevious || state["state"] == "stop"
	if ended && r.last["state"] == "play" && !r.statsSeeked && !r.statsCounted && length > 0 && length < 15000 && remaining >= 0 && elapsed >= remaining-0.05 && r.statsSeconds+remaining >= length/1000-0.25 {
		r.countItem(r.statsItem, now)
	}
	if item != r.statsItem || progress+500 < r.statsPrevious {
		r.statsItem = item
		r.statsSeconds = 0
		r.statsSeeked = false
		r.statsCounted = false
		r.statsPrevious = 0
		r.statsAt = now
		return
	}
	delta := (progress - r.statsPrevious) / 1000
	if state["state"] == "play" && !r.statsAt.IsZero() && delta > 0 && delta <= elapsed+0.25 {
		r.statsSeconds += delta
	}
	r.statsPrevious = progress
	r.statsAt = now
	if r.statsCounted || r.statsSeconds < 15 {
		return
	}
	r.countItem(item, now)
}
func (r *Remote) countItem(item float64, now time.Time) {
	index := -1
	for i, id := range r.saved.ItemIDs {
		if id == item {
			index = i
		}
	}
	if index < 0 || index >= len(r.saved.Queue) {
		return
	}
	id := r.saved.Queue[index]
	if e := r.app.store.Update(func(st *State) error {
		for i := range st.Tracks {
			if st.Tracks[i].ID == id && !st.Tracks[i].Missing {
				st.Tracks[i].PlayCount++
				st.Tracks[i].LastPlayed = now.UnixMilli()
				trimRecent(st)
				break
			}
		}
		return nil
	}); e == nil {
		r.statsCounted = true
	}
}
