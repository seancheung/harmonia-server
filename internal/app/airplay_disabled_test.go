package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDisableAirPlayEnvironment(t *testing.T) {
	for _, value := range []string{"", "false", "0", "true", "1", "TRUE", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HARMONIA_DISABLE_AIRPLAY", value)
			want := value == "true" || value == "1" || value == "TRUE"
			if ConfigFromEnv().DisableAirPlay != want {
				t.Fatalf("unexpected setting for %q", value)
			}
		})
	}
}

func TestAirPlayDisabled(t *testing.T) {
	data := t.TempDir()
	// Disabled startup must ignore and preserve even unreadable remote state.
	for _, name := range []string{"remote.json", "airplay-identity.json"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("invalid saved state"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := New(Config{DataDir: data, DisableAirPlay: true, FFmpeg: "missing-ffmpeg", FFprobe: "missing-ffprobe"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.remote != nil {
		t.Fatal("AirPlay initialized while disabled")
	}
	for _, path := range []string{"/api/health", "/api/outputs", "/api/remote", "/api/capabilities"} {
		res := request(t, a, "GET", path, nil)
		if res.Code != 200 {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		switch path {
		case "/api/outputs":
			if len(body["outputs"].([]any)) != 0 || len(body["protocols"].([]any)) != 0 {
				t.Fatal(body)
			}
		case "/api/remote":
			if body["configured"] != false || body["player"].(map[string]any)["state"] != "stop" {
				t.Fatal(body)
			}
		case "/api/capabilities":
			if body["airplay"] != false || len(body["airplayProtocols"].([]any)) != 0 || body["airplayMaxOutputs"] != float64(0) {
				t.Fatal(body)
			}
		}
	}
	for _, action := range []string{"play", "outputs", "pair", "queue"} {
		if res := request(t, a, "POST", "/api/remote", map[string]any{"action": action}); res.Code != 503 {
			t.Fatalf("%s: %d", action, res.Code)
		}
	}
	a.Close()
	for _, name := range []string{"remote.json", "airplay-identity.json"} {
		b, err := os.ReadFile(filepath.Join(data, name))
		if err != nil || string(b) != "invalid saved state" {
			t.Fatalf("saved state changed: %s, %v", name, err)
		}
	}
}
