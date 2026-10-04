package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAudioInfoMatchesActualStream(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	a := testApp(t)
	root := t.TempDir()
	path := filepath.Join(root, "test.wav")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1", "-c:a", "pcm_s24le", path).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	gain := -6.0
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Path: root}}
		st.Tracks = []Track{{ID: "song", SourceID: "source", Path: "test.wav", Format: "wav", Revision: "v1", TrackGain: &gain}}
		st.RuleSets = []RuleSet{{ID: "aac", Rules: []Conversion{{Codec: "aac", OutputBitrate: 128, OutputSampleRate: 44100}}}, {ID: "unmatched", Rules: []Conversion{{Format: "flac", Codec: "mp3"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	identities := map[string]string{}
	for _, test := range []struct {
		query, codec string
		rate, depth  int
		converted    bool
	}{
		{"", "pcm_s24le", 48000, 24, false},
		{"?ruleSet=unmatched", "pcm_s24le", 48000, 24, false},
		{"?ruleSet=aac", "aac", 44100, 0, true},
		{"?output=airplay&gain=track", "pcm_s16le", 44100, 16, true},
	} {
		t.Run(test.query, func(t *testing.T) {
			res := request(t, a, "GET", "/api/tracks/song/audio-info"+test.query, nil)
			if res.Code != 200 {
				t.Fatalf("info: %d %s", res.Code, res.Body.String())
			}
			var value struct {
				TrackID        string `json:"trackId"`
				Revision       string `json:"revision"`
				Identity       string `json:"audioIdentity"`
				Transcoded     bool   `json:"transcoded"`
				Source, Output AudioInfo
			}
			if err := json.Unmarshal(res.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if value.TrackID != "song" || value.Revision != "v1" || value.Transcoded != test.converted || value.Output.Codec != test.codec || value.Output.SampleRate != test.rate || value.Output.BitDepth != test.depth || value.Output.Bitrate <= 0 || value.Source.BitDepth != 24 {
				t.Fatalf("unexpected info: %+v", value)
			}
			identities[test.query] = value.Identity
			stream := request(t, a, "GET", "/api/tracks/song/stream"+test.query, nil)
			if stream.Code != 200 {
				t.Fatalf("stream: %d", stream.Code)
			}
			actualPath := filepath.Join(t.TempDir(), "audio")
			if err := os.WriteFile(actualPath, stream.Body.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			actual, err := probeAudioInfo(context.Background(), "ffprobe", actualPath)
			if err != nil || actual != value.Output {
				t.Fatalf("stream mismatch: %+v %+v %v", actual, value.Output, err)
			}
		})
	}
	if identities[""] != identities["?ruleSet=unmatched"] || identities[""] == identities["?ruleSet=aac"] {
		t.Fatal("incorrect quality identities")
	}
	if res := request(t, a, "GET", "/api/tracks/missing/audio-info", nil); res.Code != 404 {
		t.Fatalf("missing: %d", res.Code)
	}
	a.config.FFprobe = filepath.Join(root, "absent-probe")
	if res := request(t, a, "GET", "/api/tracks/song/audio-info", nil); res.Code != 422 {
		t.Fatalf("probe failure: %d", res.Code)
	}
}
