package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// AudioInfo describes the probed bytes, never the conversion rule's target values.
type AudioInfo struct {
	Codec         string  `json:"codec"`
	Container     string  `json:"container"`
	SampleRate    int     `json:"sampleRate,omitempty"`
	Channels      int     `json:"channels,omitempty"`
	BitDepth      int     `json:"bitDepth,omitempty"`
	Bitrate       int64   `json:"bitrate,omitempty"` // bits/second, not kbps
	BitrateKind   string  `json:"bitrateKind,omitempty"`
	BitrateSource string  `json:"bitrateSource,omitempty"`
	Duration      float64 `json:"duration,omitempty"`
}

func probeAudioInfo(ctx context.Context, executable, path string) (AudioInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, executable, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,sample_rate,channels,bits_per_sample,bits_per_raw_sample,bit_rate,duration:format=format_name,duration,size", "-of", "json", path).Output()
	if err != nil {
		return AudioInfo{}, err
	}
	var result struct {
		Streams []struct {
			Codec      string `json:"codec_name"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
			Bits       int    `json:"bits_per_sample"`
			RawBits    string `json:"bits_per_raw_sample"`
			Bitrate    string `json:"bit_rate"`
			Duration   string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return AudioInfo{}, err
	}
	if len(result.Streams) == 0 {
		return AudioInfo{}, exec.ErrNotFound
	}
	stream := result.Streams[0]
	info := AudioInfo{Codec: stream.Codec, Container: result.Format.Name, Channels: stream.Channels}
	info.SampleRate, _ = strconv.Atoi(stream.SampleRate)
	info.Duration, _ = strconv.ParseFloat(stream.Duration, 64)
	if info.Duration <= 0 {
		info.Duration, _ = strconv.ParseFloat(result.Format.Duration, 64)
	}
	if strings.HasPrefix(info.Codec, "pcm_") || info.Codec == "flac" || info.Codec == "alac" || info.Codec == "wavpack" || info.Codec == "ape" {
		info.BitDepth, _ = strconv.Atoi(stream.RawBits)
		if info.BitDepth <= 0 {
			info.BitDepth = stream.Bits
		}
	}
	info.Bitrate, _ = strconv.ParseInt(stream.Bitrate, 10, 64)
	if info.Bitrate > 0 {
		info.BitrateKind = "average"
		info.BitrateSource = "audioStream"
	} else if info.Duration > 0 {
		size, _ := strconv.ParseInt(result.Format.Size, 10, 64)
		if size > 0 {
			info.Bitrate = int64(float64(size) * 8 / info.Duration)
			info.BitrateKind = "estimatedAverage"
			info.BitrateSource = "containerSize"
		}
	}
	return info, nil
}

func (a *App) audioInfo(w http.ResponseWriter, r *http.Request) {
	track, path, err := a.track(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "track unavailable"})
		return
	}
	rule, gain := a.streamConversion(track, r)
	source, err := probeAudioInfo(r.Context(), a.config.FFprobe, path)
	if err != nil {
		respond(w, 422, map[string]string{"error": "audio information unavailable"})
		return
	}
	actual := source
	if rule != nil {
		var release func()
		path, release, err = a.cache.Acquire(r.Context(), track, path, *rule, gain)
		if err != nil {
			respond(w, 422, map[string]string{"error": "conversion unavailable"})
			return
		}
		defer release()
		actual, err = probeAudioInfo(r.Context(), a.config.FFprobe, path)
		if err != nil {
			respond(w, 422, map[string]string{"error": "output audio information unavailable"})
			return
		}
	}
	identity, _ := json.Marshal([]any{track.ID, track.Revision, track.Modified, track.Size, rule, gain})
	hash := sha256.Sum256(identity)
	w.Header().Set("Cache-Control", "private, no-store")
	respond(w, 200, map[string]any{"trackId": track.ID, "revision": track.Revision, "audioIdentity": hex.EncodeToString(hash[:]), "transcoded": rule != nil, "source": source, "output": actual})
}
