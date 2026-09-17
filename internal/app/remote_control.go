package app

import (
	"errors"
	"fmt"
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
		err = r.resumePlayback()
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
		r.saved.ResumePosition = nil
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

// Defer selection and seeking for paused transfers until explicit playback.
func (r *Remote) resumePlayback() error {
	r.mu.Lock()
	saved := r.saved
	pending := r.pending
	if pending != nil {
		saved = *pending
		if saved.ResumePosition != nil && saved.Index >= len(saved.ItemIDs) {
			// The queue worker will start the selected track once it is available.
			r.mu.Unlock()
			return nil
		}
	}
	r.mu.Unlock()
	path := "player/play"
	if saved.ResumePosition != nil {
		path = fmt.Sprintf("player/play?position=%d", saved.Index)
	}
	if _, err := r.call("PUT", path, nil); err != nil {
		return err
	}
	if saved.ResumePosition != nil {
		if *saved.ResumePosition > 0 {
			if _, err := r.call("PUT", fmt.Sprintf("player/seek?position_ms=%d", *saved.ResumePosition), nil); err != nil {
				return err
			}
		}
		r.mu.Lock()
		if pending != nil {
			pending.ResumePosition = nil
		} else {
			r.saved.ResumePosition = nil
		}
		r.mu.Unlock()
	}
	return nil
}
