package app

import (
	"errors"
	"net/http"
)

var errQueueCancelled = errors.New("queue submission cancelled")

// Do not cancel an in-flight OwnTone add: disconnecting cannot cancel its side
// effects. Stop subsequent adds and clear again once that request finishes.
func (r *Remote) queueCheckpoint() error {
	r.mu.Lock()
	cancelled := r.cancelQueue
	r.mu.Unlock()
	if !cancelled {
		return nil
	}
	r.controls.Lock()
	defer r.controls.Unlock()
	if _, err := r.call("PUT", "queue/clear", nil); err != nil {
		return &queueOutcomeUnknown{err}
	}
	return errQueueCancelled
}

func (r *Remote) controlDuringQueue(w http.ResponseWriter, action string) {
	r.controls.Lock()
	defer r.controls.Unlock()
	r.mu.Lock()
	r.holdPlayback = action != "play"
	if action == "clear" {
		r.cancelQueue = true
	}
	r.mu.Unlock()
	var err error
	if action == "clear" {
		_, err = r.call("PUT", "queue/clear", nil)
	} else if action == "play" {
		_, err = r.call("PUT", "player/play", nil)
	} else {
		err = r.pauseForLocal()
	}
	if err != nil {
		respond(w, 502, map[string]string{"error": err.Error()})
		return
	}
	r.mu.Lock()
	if action == "clear" {
		r.saved.Queue, r.saved.ItemIDs, r.saved.Index = []string{}, []float64{}, 0
		if r.pending != nil {
			r.pending.Queue, r.pending.ItemIDs, r.pending.Index = []string{}, []float64{}, 0
		}
	}
	if action == "local" || action == "clear" {
		r.deadline, r.waiting, r.finish = 0, false, false
	}
	r.mu.Unlock()
	if state, e := r.call("GET", "player", nil); e == nil {
		r.mu.Lock()
		r.last = state
		r.mu.Unlock()
	}
	r.persist()
	respond(w, 200, map[string]bool{"ok": true})
}
