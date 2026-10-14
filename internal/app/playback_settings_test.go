package app

import (
	"encoding/json"
	"testing"
)

func TestPlaybackSettingsWithoutAirPlay(t *testing.T) {
	config := Config{DataDir: t.TempDir(), DisableAirPlay: true, FFmpeg: "missing", FFprobe: "missing"}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res := request(t, a, "GET", "/api/playback-settings", nil)
	var settings PlaybackSettings
	if err := json.Unmarshal(res.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if res.Code != 200 || settings.Gain != "off" || !settings.Protect || settings.Failure != "stop" {
		t.Fatal(res.Body.String())
	}
	desired := PlaybackSettings{Gain: "auto", Preamp: -3, Protect: true, Failure: "skip"}
	res = request(t, a, "PUT", "/api/playback-settings", desired)
	if res.Code != 200 || a.currentPlaybackSettings() != desired {
		t.Fatal(res.Body.String())
	}
	invalid := desired
	invalid.Preamp = 31
	invalid.Failure = "stop"
	if request(t, a, "PUT", "/api/playback-settings", invalid).Code != 400 || a.currentPlaybackSettings() != desired {
		t.Fatal("invalid group partially saved")
	}
	a.Close()
	restored, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.currentPlaybackSettings() != desired {
		t.Fatal("settings not persisted")
	}
}
func TestPlaybackSettingsAppliesWholeGroupToRemote(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	session := a.remote.saved.PlaybackSession
	a.remote.mu.Unlock()
	desired := PlaybackSettings{Gain: "track", Preamp: -6, Protect: true, Failure: "skip"}
	res := request(t, a, "PUT", "/api/playback-settings", desired)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	awaitSession(t, sessions)
	a.remote.mu.Lock()
	defer a.remote.mu.Unlock()
	if a.remote.saved.Gain != "track" || a.remote.saved.Preamp != -6 || a.remote.saved.Failure != "skip" || a.remote.saved.PlaybackSession != session {
		t.Fatal("group not applied or playback reset")
	}
}
