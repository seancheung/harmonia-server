package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlsAndPollingDuringQueueSubmission(t *testing.T) {
	var mu sync.Mutex
	items := []map[string]any{}
	adds, plays := 0, 0
	state := "stop"
	blocked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/queue/items/add" {
			mu.Lock()
			adds++
			number := adds
			mu.Unlock()
			if number == 2 {
				close(blocked)
				<-release
			}
			mu.Lock()
			items = append(items, map[string]any{"id": number, "uri": req.URL.Query().Get("uris")})
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]int{"count": 1})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/api/queue":
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case "/api/player":
			json.NewEncoder(w).Encode(map[string]any{"state": state, "item_id": 1, "item_progress_ms": 12345})
		case "/api/player/play":
			state = "play"
			plays++
			w.WriteHeader(204)
		case "/api/player/pause":
			state = "pause"
			w.WriteHeader(204)
		case "/api/queue/clear":
			items = nil
			state = "stop"
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer own.Close()
	defer unblock()
	r := NewRemote(&App{config: Config{OwnTone: own.URL, DataDir: t.TempDir()}})
	r.commands.Lock()
	defer r.commands.Unlock()
	defer r.Close()
	r.mu.Lock()
	r.pending = &RemoteSaved{Queue: []string{"one", "two", "three"}}
	r.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- r.replaceURLs([]string{"http://one", "http://two", "http://three"}, 0) }()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("second add did not begin")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		progress := r.last["item_progress_ms"]
		r.mu.Unlock()
		if progress == float64(12345) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("progress polling blocked by queue submission")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, action := range []string{"pause", "play", "clear"} {
		w := httptest.NewRecorder()
		r.Command(w, httptest.NewRequest("POST", "/api/remote", strings.NewReader(`{"action":"`+action+`"}`)))
		if w.Code != 200 {
			t.Fatalf("%s failed: %s", action, w.Body.String())
		}
		mu.Lock()
		current := state
		mu.Unlock()
		expected := map[string]string{"pause": "pause", "play": "play", "clear": "stop"}[action]
		if current != expected {
			t.Fatalf("%s did not take effect", action)
		}
	}
	unblock()
	select {
	case err := <-result:
		if !errors.Is(err, errQueueCancelled) {
			t.Fatalf("unexpected result: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if adds != 2 || len(items) != 0 || plays != 2 {
		t.Fatalf("queue resumed after clear: adds=%d items=%d plays=%d", adds, len(items), plays)
	}
}
