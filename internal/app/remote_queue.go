package app

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

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
		for end < len(uris) && end-start < 20 {
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
			// Inspect after an ambiguous timeout too: the request may have succeeded.
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
	}
	return nil
}

func (r *Remote) replaceURLs(uris []string, index int) error {
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
		err = r.addBatches(uris, 0)
		if err == nil {
			_, err = r.call("PUT", "player/play?position="+strconv.Itoa(index), nil)
		}
	}
	if err == nil {
		return nil
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
