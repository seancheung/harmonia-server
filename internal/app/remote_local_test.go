package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPauseForLocal(t *testing.T) {
	for _, tc := range []struct {
		name, before, after   string
		pauseFails, wantError bool
		wantPauses            int
	}{
		{"stopped", "stop", "stop", true, false, 0},
		{"paused", "pause", "pause", true, false, 0},
		{"playing", "play", "pause", false, false, 1},
		{"ended during pause", "play", "stop", true, false, 1},
		{"still playing", "play", "play", true, true, 1},
		{"unknown state", "unknown", "unknown", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pauses := 0
			own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "GET" && req.URL.Path == "/api/player" {
					state := tc.before
					if pauses > 0 {
						state = tc.after
					}
					json.NewEncoder(w).Encode(map[string]string{"state": state})
					return
				}
				if req.Method == "PUT" && req.URL.Path == "/api/player/pause" {
					pauses++
					if tc.pauseFails {
						http.Error(w, "Error pausing playback", 500)
					} else {
						w.WriteHeader(204)
					}
					return
				}
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			}))
			defer own.Close()
			r := &Remote{app: &App{config: Config{OwnTone: own.URL}}, client: own.Client()}
			err := r.pauseForLocal()
			if (err != nil) != tc.wantError || pauses != tc.wantPauses {
				t.Fatalf("err=%v, pauses=%d", err, pauses)
			}
		})
	}
}
