package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type queueTransport func(*http.Request) (*http.Response, error)

func (f queueTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestQueueTimeoutDoesNotRollbackOrRetry(t *testing.T) {
	clears, adds := 0, 0
	client := &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch req.URL.Path {
		case "/api/queue":
			body = `{"items":[]}`
		case "/api/player":
		case "/api/queue/clear":
			clears++
		case "/api/queue/items/add":
			adds++
			if len(strings.Split(req.URL.Query().Get("uris"), ",")) != 1 {
				t.Fatal("cold tracks must be submitted individually")
			}
			return nil, context.DeadlineExceeded
		default:
			t.Fatalf("unexpected recovery request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	r := &Remote{app: &App{config: Config{OwnTone: "http://own"}}, client: client}
	err := r.replaceURLs([]string{"http://track/1", "http://track/2"}, 0)
	var unknown *queueOutcomeUnknown
	if !errors.As(err, &unknown) || clears != 1 || adds != 1 {
		t.Fatalf("ambiguous request was retried or rolled back: %v, clears=%d adds=%d", err, clears, adds)
	}
}

func TestQueueBatches(t *testing.T) {
	uris := make([]string, 1000)
	for i := range uris {
		uris[i] = fmt.Sprintf("http://localhost/api/tracks/%d/stream?token=%s&gain=track", i, strings.Repeat("x", 80))
	}
	paths, err := queueBatches(uris, 7)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, path := range paths {
		if len(path) > 6000 {
			t.Fatal("oversized request")
		}
		q, _ := url.ParseQuery(strings.SplitN(path, "?", 2)[1])
		position, _ := strconv.Atoi(q.Get("position"))
		if position != 7+len(got) {
			t.Fatal("incorrect insertion position")
		}
		got = append(got, strings.Split(q.Get("uris"), ",")...)
	}
	if !reflect.DeepEqual(got, uris) {
		t.Fatal("queue order or count changed")
	}
	if _, err = queueBatches([]string{strings.Repeat("x", 7000)}, 0); err == nil {
		t.Fatal("oversized item accepted")
	}
}

func TestRemoteLargeQueueAndRecovery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			items := []map[string]any{{"id": 1, "uri": "http://old/one"}, {"id": 2, "uri": "http://old/two"}}
			nextID, batches, playingIndex, progress := 3, 0, 1, 12000
			playing := "play"
			own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				q := req.URL.Query()
				switch {
				case req.URL.Path == "/api/queue":
					start, _ := strconv.Atoi(q.Get("start"))
					end, _ := strconv.Atoi(q.Get("end"))
					start = min(start, len(items))
					end = min(end, len(items))
					_ = json.NewEncoder(w).Encode(map[string]any{"items": items[start:end], "count": len(items)})
				case req.URL.Path == "/api/player":
					id := 0
					if len(items) > 0 {
						id = items[min(playingIndex, len(items)-1)]["id"].(int)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"state": playing, "item_id": id, "item_progress_ms": progress})
				case req.URL.Path == "/api/queue/clear":
					items = nil
					w.WriteHeader(204)
				case req.URL.Path == "/api/queue/items/add":
					batches++
					uris := strings.Split(q.Get("uris"), ",")
					if len(req.RequestURI) > 6010 {
						t.Error("oversized request target")
					}
					position, _ := strconv.Atoi(q.Get("position"))
					added := []map[string]any{}
					for _, uri := range uris {
						added = append(added, map[string]any{"id": nextID, "uri": uri})
						nextID++
					}
					tail := append([]map[string]any{}, items[position:]...)
					items = append(append(items[:position], added...), tail...)
					// Simulate a request which mutated the queue but returned an error.
					if fail && batches == 3 {
						http.Error(w, "injected failure", 500)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"count": len(uris)})
				case strings.HasPrefix(req.URL.Path, "/api/queue/items/") && req.Method == "DELETE":
					id, _ := strconv.Atoi(strings.TrimPrefix(req.URL.Path, "/api/queue/items/"))
					for i, item := range items {
						if item["id"] == id {
							items = append(items[:i], items[i+1:]...)
							break
						}
					}
					w.WriteHeader(204)
				case req.URL.Path == "/api/player/play":
					playingIndex, _ = strconv.Atoi(q.Get("position"))
					if !fail && len(items) != playingIndex+1 {
						t.Error("playback waited for tracks after the selected item")
					}
					playing = "play"
					w.WriteHeader(204)
				case req.URL.Path == "/api/player/seek":
					progress, _ = strconv.Atoi(q.Get("position_ms"))
					w.WriteHeader(204)
				case req.URL.Path == "/api/player/pause":
					playing = "pause"
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request %s", req.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer own.Close()
			r := &Remote{app: &App{config: Config{OwnTone: own.URL, DataDir: t.TempDir()}}, client: own.Client()}
			uris := make([]string, 500)
			for i := range uris {
				uris[i] = fmt.Sprintf("http://new/%d", i)
			}
			err := r.replaceURLs(uris, 450)
			if fail {
				if err == nil || len(items) != 2 || items[0]["uri"] != "http://old/one" || items[1]["uri"] != "http://old/two" || playingIndex != 1 || progress != 12000 || playing != "play" {
					t.Fatalf("recovery failed: %v, %v", err, items)
				}
			} else {
				if err != nil || len(items) != 500 || playingIndex != 450 {
					t.Fatalf("large queue failed: %v (%d items)", err, len(items))
				}
				for i, item := range items {
					if item["uri"] != uris[i] {
						t.Fatal("queue order changed")
					}
				}
			}
		})
	}
}

func TestRemoteStatusOmitsUnchangedQueue(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.remote.mu.Lock()
	a.remote.saved.Queue = []string{"missing"}
	a.remote.mu.Unlock()
	first := request(t, a, "GET", "/api/remote", nil)
	var status map[string]any
	if err = json.Unmarshal(first.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	version := status["queueVersion"].(string)
	next := request(t, a, "GET", "/api/remote?queueVersion="+version, nil)
	status = map[string]any{}
	_ = json.Unmarshal(next.Body.Bytes(), &status)
	if _, ok := status["queue"]; ok {
		t.Fatal("unchanged queue retransmitted")
	}
	a.remote.mu.Lock()
	a.remote.saved.Queue = append(a.remote.saved.Queue, "new")
	a.remote.mu.Unlock()
	next = request(t, a, "GET", "/api/remote?queueVersion="+version, nil)
	_ = json.Unmarshal(next.Body.Bytes(), &status)
	if len(status["queue"].([]any)) != 2 {
		t.Fatal("queue change not transmitted")
	}
}
