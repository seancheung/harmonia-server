package app

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
)

type PlaybackSettings struct {
	Gain    string  `json:"gain"`
	Preamp  float64 `json:"preamp"`
	Protect bool    `json:"protect"`
	Failure string  `json:"failure"`
}

func (s PlaybackSettings) validate() error {
	if s.Gain != "off" && s.Gain != "auto" && s.Gain != "track" && s.Gain != "album" {
		return errors.New("invalid gain mode")
	}
	if math.IsNaN(s.Preamp) || math.IsInf(s.Preamp, 0) || s.Preamp < -30 || s.Preamp > 30 {
		return errors.New("invalid preamp")
	}
	if s.Failure != "stop" && s.Failure != "skip" {
		return errors.New("invalid failure policy")
	}
	return nil
}
func (a *App) loadPlaybackSettings() error {
	settings := PlaybackSettings{Gain: "off", Protect: true, Failure: "stop"}
	data, err := os.ReadFile(filepath.Join(a.config.DataDir, "playback-settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		// Legacy playback state is an optional migration source, never a
		// dependency for configuring playback while AirPlay is disabled.
		legacy, legacyErr := os.ReadFile(filepath.Join(a.config.DataDir, "remote.json"))
		migrated := settings
		if legacyErr == nil && json.Unmarshal(legacy, &migrated) == nil {
			if migrated.Gain == "" {
				migrated.Gain = "off"
			}
			if migrated.Failure == "" {
				migrated.Failure = "stop"
			}
			if migrated.validate() == nil {
				settings = migrated
			}
		}
		a.playbackSettings = settings
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(data, &settings); err != nil {
			return err
		}
	}
	if settings.Gain == "" {
		settings.Gain = "off"
	}
	if settings.Failure == "" {
		settings.Failure = "stop"
	}
	if err = settings.validate(); err != nil {
		return err
	}
	a.playbackSettings = settings
	return nil
}
func (a *App) currentPlaybackSettings() PlaybackSettings {
	a.playbackMu.Lock()
	defer a.playbackMu.Unlock()
	return a.playbackSettings
}
func (a *App) getPlaybackSettings(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, a.currentPlaybackSettings())
}
func (a *App) putPlaybackSettings(w http.ResponseWriter, r *http.Request) {
	var settings PlaybackSettings
	if !decode(w, r, &settings) {
		return
	}
	if a.remote != nil {
		a.remote.commands.Lock()
		defer a.remote.commands.Unlock()
	}
	if err := a.savePlaybackSettings(settings); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	respond(w, 200, settings)
}

// Caller holds remote.commands when AirPlay is enabled. Persist the whole group
// before applying it; an unavailable receiver cannot prevent saving preferences.
func (a *App) savePlaybackSettings(settings PlaybackSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	a.playbackMu.Lock()
	err := atomicJSON(filepath.Join(a.config.DataDir, "playback-settings.json"), settings)
	if err == nil {
		a.playbackSettings = settings
	}
	a.playbackMu.Unlock()
	if err != nil {
		return err
	}
	if r := a.remote; r != nil {
		r.mu.Lock()
		restart := (r.state == "play" || r.state == "loading") && (r.saved.Gain != settings.Gain || r.saved.Preamp != settings.Preamp || r.saved.Protect != settings.Protect)
		r.saved.Gain, r.saved.Preamp, r.saved.Protect, r.saved.Failure = settings.Gain, settings.Preamp, settings.Protect, settings.Failure
		r.mu.Unlock()
		if restart {
			r.cancelPlayback()
			if err := r.startWithPolicy(); err != nil {
				r.mu.Lock()
				r.lastError = err.Error()
				r.mu.Unlock()
			}
		}
		// remote.json is a playback snapshot; playback-settings.json is authoritative.
		_ = r.persist()
	}
	return nil
}
