package app

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// A lost response does not cancel OwnTone's work. Never roll back while it may
// still be adding items, and never retry a non-idempotent add automatically.
type queueOutcomeUnknown struct{ cause error }

func (e *queueOutcomeUnknown) Error() string {
	return "AirPlay queue outcome unknown; OwnTone may still be adding tracks: " + e.cause.Error()
}
func (e *queueOutcomeUnknown) Unwrap() error { return e.cause }

// Fetch bounded pages rather than decoding an unbounded metadata response.
func (r *Remote) queueItems() ([]any, error) {
	items := []any{}
	for start := 0; ; start += 100 {
		result, err := r.call("GET", fmt.Sprintf("queue?start=%d&end=%d", start, start+100), nil)
		if err != nil {
			return nil, err
		}
		page, _ := result["items"].([]any)
		items = append(items, page...)
		if len(page) < 100 {
			return items, nil
		}
	}
}

// Bound the encoded request target, including escaped stream parameters.
func queueBatches(uris []string, position int) ([]string, error) {
	paths := []string{}
	for start := 0; start < len(uris); {
		end := start
		path := ""
		for end < len(uris) && end-start < 1 {
			q := url.Values{"uris": {strings.Join(uris[start:end+1], ",")}, "position": {strconv.Itoa(position + start)}}
			candidate := "queue/items/add?" + q.Encode()
			if len(candidate) > 6000 {
				break
			}
			path = candidate
			end++
		}
		if end == start {
			return nil, errors.New("stream URL exceeds queue request limit")
		}
		paths = append(paths, path)
		start = end
	}
	return paths, nil
}

func (r *Remote) addBatches(uris []string, position int) error {
	return r.addBatchesWithProgress(uris, position, nil)
}

func (r *Remote) addBatchesWithProgress(uris []string, position int, progress func(int) error) error {
	paths, err := queueBatches(uris, position)
	if err != nil || len(paths) == 0 {
		return err
	}
	before, err := r.queueItems()
	if err != nil {
		return err
	}
	original := map[float64]bool{}
	for _, item := range before {
		if m, ok := item.(map[string]any); ok {
			if id, ok := m["id"].(float64); ok {
				original[id] = true
			}
		}
	}
	added := 0
	for _, path := range paths {
		result, callErr := r.call("POST", path, nil)
		if callErr == nil {
			query, _ := url.ParseQuery(strings.SplitN(path, "?", 2)[1])
			expected := len(strings.Split(query.Get("uris"), ","))
			if count, ok := result["count"].(float64); ok && int(count) != expected {
				callErr = errors.New("OwnTone added an incomplete queue batch")
			}
		}
		if callErr != nil {
			var unknown *queueOutcomeUnknown
			if errors.As(callErr, &unknown) {
				return callErr
			}
			after, readErr := r.queueItems()
			if readErr != nil {
				return errors.Join(callErr, fmt.Errorf("queue recovery: %w", readErr))
			}
			for _, item := range after {
				if m, ok := item.(map[string]any); ok {
					if id, ok := m["id"].(float64); ok && !original[id] {
						_, cleanupErr := r.call("DELETE", fmt.Sprintf("queue/items/%.0f", id), nil)
						callErr = errors.Join(callErr, cleanupErr)
					}
				}
			}
			return callErr
		}
		query, _ := url.ParseQuery(strings.SplitN(path, "?", 2)[1])
		added += len(strings.Split(query.Get("uris"), ","))
		if progress != nil {
			if err := progress(added); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Remote) replaceURLs(uris []string, index int, positionMS ...int) error {
	// Validate all batches and capture a recoverable snapshot before clearing.
	if _, err := queueBatches(uris, 0); err != nil {
		return err
	}
	old, err := r.queueItems()
	if err != nil {
		return err
	}
	state, err := r.call("GET", "player", nil)
	if err != nil {
		return err
	}
	oldURIs := []string{}
	oldIndex := 0
	for i, item := range old {
		m, ok := item.(map[string]any)
		if !ok {
			return errors.New("cannot snapshot OwnTone queue")
		}
		uri, _ := m["uri"].(string)
		if uri == "" {
			return errors.New("cannot restore OwnTone item without URI")
		}
		oldURIs = append(oldURIs, uri)
		if m["id"] == state["item_id"] {
			oldIndex = i
		}
	}
	if _, err = r.call("PUT", "queue/clear", nil); err == nil {
		started := false
		err = r.addBatchesWithProgress(uris, 0, func(added int) error {
			if started || added <= index {
				return nil
			}
			_, playErr := r.call("PUT", "player/play?position="+strconv.Itoa(index), nil)
			if playErr == nil && len(positionMS) > 0 && positionMS[0] > 0 {
				_, playErr = r.call("PUT", fmt.Sprintf("player/seek?position_ms=%d", positionMS[0]), nil)
			}
			started = playErr == nil
			return playErr
		})
	}
	if err == nil {
		return nil
	}
	var unknown *queueOutcomeUnknown
	if errors.As(err, &unknown) {
		return err
	}
	_, recovery := r.call("PUT", "queue/clear", nil)
	if recovery == nil {
		recovery = r.addBatches(oldURIs, 0)
	}
	if recovery == nil && len(oldURIs) > 0 {
		_, recovery = r.call("PUT", "player/play?position="+strconv.Itoa(oldIndex), nil)
		if recovery == nil {
			if position, ok := state["item_progress_ms"].(float64); ok {
				_, recovery = r.call("PUT", fmt.Sprintf("player/seek?position_ms=%.0f", position), nil)
			}
		}
		if state["state"] != "play" {
			_, pauseErr := r.call("PUT", "player/pause", nil)
			recovery = errors.Join(recovery, pauseErr)
		}
	}
	if recovery == nil {
		recovery = r.mapQueue()
		r.persist()
	}
	if recovery != nil {
		return errors.Join(err, fmt.Errorf("queue recovery failed: %w", recovery))
	}
	return err
}
