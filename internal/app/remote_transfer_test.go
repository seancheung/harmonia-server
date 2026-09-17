package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPausedRemoteTransfer(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Path: root}}
		st.Tracks = []Track{{ID: "one", SourceID: "source"}, {ID: "two", SourceID: "source"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{}
	plays, seeks := []string{}, []string{}
	state := "stop"
	current := 1
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/queue":
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case "/api/player":
			json.NewEncoder(w).Encode(map[string]any{"state": state, "item_id": current, "item_progress_ms": 0})
		case "/api/queue/clear":
			items = nil
			state = "stop"
			w.WriteHeader(204)
		case "/api/queue/items/add":
			items = append(items, map[string]any{"id": len(items) + 1, "uri": req.URL.Query().Get("uris")})
			json.NewEncoder(w).Encode(map[string]int{"count": 1})
		case "/api/player/play":
			plays = append(plays, req.URL.RawQuery)
			state, current = "play", 2
			w.WriteHeader(204)
		case "/api/player/seek":
			seeks = append(seeks, req.URL.RawQuery)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s", req.URL)
			w.WriteHeader(404)
		}
	}))
	defer own.Close()
	// Use a Remote without a polling goroutine so intermediate states are deterministic.
	config := a.config
	config.OwnTone = own.URL
	r := &Remote{app: &App{config: config, store: a.store}, client: own.Client()}
	command := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		r.Command(w, httptest.NewRequest("POST", "/remote", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("command: %d %s", w.Code, w.Body.String())
		}
	}
	command(`{"action":"start","ids":["one","two"],"index":1,"position":12500,"playing":false}`)
	if len(plays) != 0 {
		t.Fatalf("paused transfer started playback: %v", plays)
	}
	w := httptest.NewRecorder()
	r.Status(w, httptest.NewRequest("GET", "/remote", nil))
	var status struct {
		Index  int
		Player struct {
			State    string
			Position int `json:"item_progress_ms"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Index != 1 || status.Player.State != "pause" || status.Player.Position != 12500 {
		t.Fatalf("status: %s", w.Body.String())
	}
	command(`{"action":"play"}`)
	if len(plays) != 1 || plays[0] != "position=1" || len(seeks) != 1 || seeks[0] != "position_ms=12500" {
		t.Fatalf("resume: plays=%v seeks=%v", plays, seeks)
	}
	if r.saved.ResumePosition != nil {
		t.Fatal("resume position retained after playback")
	}
}
