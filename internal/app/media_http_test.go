package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOriginalAudioMediaRequests(t *testing.T) {
	for _, format := range []struct{ extension, contentType string }{{"mp3", "audio/mpeg"}, {"flac", "audio/flac"}} {
		t.Run(format.extension, func(t *testing.T) {
			a := testApp(t)
			root := t.TempDir()
			name := "song." + format.extension
			if err := os.WriteFile(filepath.Join(root, name), []byte("0123456789"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.store.Update(func(st *State) error {
				st.Sources = []Source{{ID: "source", Path: root}}
				st.Tracks = []Track{{ID: "song", SourceID: "source", Path: name}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// No valid decoder is needed: these responses must serve original bytes.
			a.cache.ffmpeg = filepath.Join(root, "ffmpeg-must-not-run")
			for _, query := range []string{"gain=off", "gain=track", "gain=album", "gain=off&preamp=12&protect=true"} {
				for _, method := range []string{"GET", "HEAD"} {
					req := httptest.NewRequest(method, "/api/tracks/song/stream?output=airplay&"+query, nil)
					if method == "GET" {
						req.Header.Set("Range", "bytes=0-1")
					}
					res := httptest.NewRecorder()
					a.Handler().ServeHTTP(res, req)
					wantStatus, wantBody := 200, ""
					if method == "GET" {
						wantStatus, wantBody = 206, "01"
					}
					if res.Code != wantStatus || res.Body.String() != wantBody || res.Header().Get("Content-Type") != format.contentType {
						t.Fatalf("AirPlay passthrough %s %s: %d %s", method, query, res.Code, res.Body.String())
					}
				}
			}
			gainDB := 6.0
			if err := a.store.Update(func(st *State) error {
				st.Tracks[0].TrackGain = &gainDB
				st.RuleSets = []RuleSet{{ID: "convert", Rules: []Conversion{{Codec: "wav"}}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"gain=track", "gain=off&ruleSet=convert"} {
				res := request(t, a, "GET", "/api/tracks/song/stream?output=airplay&"+query, nil)
				if res.Code != 422 {
					t.Fatalf("required conversion bypassed: %s (%d)", query, res.Code)
				}
			}
			// Gain and preamp cancel out: still serve the original even with tags.
			resUnity := request(t, a, "GET", "/api/tracks/song/stream?output=airplay&gain=track&preamp=-6", nil)
			if resUnity.Code != 200 || resUnity.Body.String() != "0123456789" {
				t.Fatal("unity gain should bypass conversion")
			}
			req := httptest.NewRequest("GET", "/api/tracks/song/stream", nil)
			req.Header.Set("Range", "bytes=0-1")
			req.Header.Set("Origin", "http://localhost:5173")
			res := httptest.NewRecorder()
			a.Handler().ServeHTTP(res, req)
			if res.Code != 206 || res.Body.String() != "01" || res.Header().Get("Content-Type") != format.contentType || res.Header().Get("Content-Range") != "bytes 0-1/10" {
				t.Fatalf("invalid media response: %d %v", res.Code, res.Header())
			}
			req = httptest.NewRequest("GET", "http://192.168.3.50:5173/api/tracks/song/stream", nil)
			req.Header.Set("Range", "bytes=0-1")
			req.Header.Set("Origin", "http://192.168.3.50:5173")
			res = httptest.NewRecorder()
			a.Handler().ServeHTTP(res, req)
			if res.Code != 206 || res.Body.String() != "01" {
				t.Fatalf("same-origin LAN media request failed: %d %s", res.Code, res.Body.String())
			}
			req = httptest.NewRequest("HEAD", "/api/tracks/song/stream", nil)
			res = httptest.NewRecorder()
			a.Handler().ServeHTTP(res, req)
			if res.Code != 200 || res.Body.Len() != 0 || res.Header().Get("Content-Type") != format.contentType {
				t.Fatal("invalid HEAD response")
			}
			req = httptest.NewRequest("OPTIONS", "/api/tracks/song/stream", nil)
			req.Header.Set("Origin", "http://localhost:5173")
			req.Header.Set("Access-Control-Request-Headers", "range")
			res = httptest.NewRecorder()
			a.Handler().ServeHTTP(res, req)
			if res.Code != 204 || res.Header().Get("Access-Control-Allow-Headers") != "Content-Type, Authorization, Range" {
				t.Fatal("range preflight failed")
			}
		})
	}
}

func TestOriginRestrictions(t *testing.T) {
	a := testApp(t)
	for _, tc := range []struct {
		origin string
		status int
	}{
		{"http://192.168.3.50:5173", 204},
		{"http://localhost:5173", 204},
		{"http://192.168.3.50:5174", 403},
		{"https://192.168.3.50:5173", 403},
		{"http://untrusted.example", 403},
		{"null", 403},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			req := httptest.NewRequest("OPTIONS", "http://192.168.3.50:5173/api/health", nil)
			req.Header.Set("Origin", tc.origin)
			res := httptest.NewRecorder()
			a.Handler().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d", res.Code, tc.status)
			}
		})
	}
}
