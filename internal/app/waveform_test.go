package app

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWaveformPCMChunkBoundaries(t *testing.T) {
	pcm := &waveformPCM{total: 1024}
	data := make([]byte, 2048)
	for i := 0; i < 1024; i++ {
		value := int16(100)
		if i == 0 {
			value = -32768
		}
		if i == 1023 {
			value = 30000
		}
		binary.LittleEndian.PutUint16(data[i*2:], uint16(value))
	}
	for len(data) > 0 {
		n := min(7, len(data))
		if written, err := pcm.Write(data[:n]); err != nil || written != n {
			t.Fatal(written, err)
		}
		data = data[n:]
	}
	if pcm.samples != 1024 || pcm.hasPending || pcm.peaks[0] != 32768 || pcm.peaks[511] != 30000 || pcm.peaks[256] != 100 {
		t.Fatalf("unexpected aggregation: %#v", pcm)
	}
}

func TestWaveformEndpointCache(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	a := testApp(t)
	root := t.TempDir()
	const samples = 8000
	wav := make([]byte, 44+samples*2)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 8000)
	binary.LittleEndian.PutUint32(wav[28:], 16000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(wav[44+i*2:], uint16(int16(math.Sin(float64(i)*2*math.Pi*440/8000)*12000)))
	}
	if err := os.WriteFile(filepath.Join(root, "test.wav"), wav, 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Name: "Music", Path: root}}
		st.Tracks = []Track{{ID: "track", SourceID: "source", Path: "test.wav", Duration: 1, Revision: "one"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first := request(t, a, http.MethodGet, "/api/tracks/track/waveform", nil)
	if first.Code != 200 {
		t.Fatalf("generate: %d %s", first.Code, first.Body.String())
	}
	var data waveformData
	if err := json.Unmarshal(first.Body.Bytes(), &data); err != nil || len(data.Peaks) != 512 || data.Peaks[0] == 0 {
		t.Fatalf("invalid waveform: %v", err)
	}
	a.config.FFmpeg = filepath.Join(root, "missing-ffmpeg")
	cached := request(t, a, http.MethodGet, "/api/tracks/track/waveform", nil)
	if cached.Code != 200 || cached.Body.String() != first.Body.String() {
		t.Fatal("cache did not avoid decoding")
	}
	if err := a.store.Update(func(st *State) error { st.Tracks[0].Revision = "two"; return nil }); err != nil {
		t.Fatal(err)
	}
	stale := request(t, a, http.MethodGet, "/api/tracks/track/waveform", nil)
	if stale.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale waveform reused: %d", stale.Code)
	}
}

func TestWaveformPCMSilenceAndOverrun(t *testing.T) {
	pcm := &waveformPCM{total: 2}
	_, _ = pcm.Write([]byte{0, 0, 0, 0, 1, 0})
	if pcm.peaks[0] != 0 || pcm.peaks[511] != 1 {
		t.Fatal(pcm.peaks)
	}
}
