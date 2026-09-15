package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const waveformPoints = 512

type waveformData struct {
	Revision string `json:"revision"`
	Peaks    []int  `json:"peaks"`
}

// Reduce PCM incrementally so generating a waveform never retains decoded audio.
type waveformPCM struct {
	peaks      [waveformPoints]int
	samples    int64
	total      float64
	pending    byte
	hasPending bool
}

func (p *waveformPCM) Write(b []byte) (int, error) {
	n := len(b)
	add := func(lo, hi byte) {
		v := int(int16(binary.LittleEndian.Uint16([]byte{lo, hi})))
		if v < 0 {
			v = -v
		}
		index := min(waveformPoints-1, int(float64(p.samples)*waveformPoints/p.total))
		p.peaks[index] = max(p.peaks[index], v)
		p.samples++
	}
	if p.hasPending && len(b) > 0 {
		add(p.pending, b[0])
		b = b[1:]
		p.hasPending = false
	}
	for len(b) >= 2 {
		add(b[0], b[1])
		b = b[2:]
	}
	if len(b) > 0 {
		p.pending = b[0]
		p.hasPending = true
	}
	return n, nil
}

func (a *App) waveform(w http.ResponseWriter, r *http.Request) {
	t, path, err := a.track(r.PathValue("id"))
	if err != nil || t.Duration <= 0 || math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	revision := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("v1:%s:%s:%d:%d:%f", path, t.Revision, info.Size(), info.ModTime().UnixNano(), t.Duration))))
	cachePath := filepath.Join(a.config.DataDir, "waveforms", fmt.Sprintf("%x.json", sha256.Sum256([]byte(t.ID))))
	serve := func(data []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("ETag", fmt.Sprintf("%q", revision))
		http.ServeContent(w, r, "waveform.json", info.ModTime(), bytes.NewReader(data))
	}
	cached := func() bool {
		data, e := os.ReadFile(cachePath)
		var value waveformData
		if e == nil && json.Unmarshal(data, &value) == nil && value.Revision == revision && len(value.Peaks) == waveformPoints {
			serve(data)
			return true
		}
		return false
	}
	if cached() {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	select {
	case a.waveformSlot <- struct{}{}:
		defer func() { <-a.waveformSlot }()
	case <-ctx.Done():
		http.Error(w, "waveform unavailable", http.StatusServiceUnavailable)
		return
	}
	if cached() {
		return
	}
	pcm := &waveformPCM{total: t.Duration * 8000}
	cmd := exec.CommandContext(ctx, a.config.FFmpeg, "-nostdin", "-v", "error", "-threads", "1", "-i", path, "-map", "0:a:0", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1")
	cmd.Stdout = pcm
	if err = cmd.Run(); err != nil || pcm.samples == 0 {
		http.Error(w, "waveform unavailable", http.StatusServiceUnavailable)
		return
	}
	peaks := make([]int, waveformPoints)
	maximum := 0
	for _, v := range pcm.peaks {
		maximum = max(maximum, v)
	}
	if maximum > 0 {
		for i, v := range pcm.peaks {
			peaks[i] = int(math.Sqrt(float64(v)/float64(maximum)) * 1000)
		}
	}
	data, _ := json.Marshal(waveformData{Revision: revision, Peaks: peaks})
	// A single replaceable entry per track prevents stale revisions accumulating.
	if os.MkdirAll(filepath.Dir(cachePath), 0755) == nil {
		if os.WriteFile(cachePath+".tmp", data, 0644) == nil {
			_ = os.Rename(cachePath+".tmp", cachePath)
		}
	}
	serve(data)
}
