package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	Path    string
	Size    int64
	Used    time.Time
	Active  int
	Pending bool
}
type Cache struct {
	mu          sync.Mutex
	entries     map[string]*cacheEntry
	locks       map[string]*sync.Mutex
	jobs        map[string]bool
	dir, ffmpeg string
	store       *Store
}

func NewCache(dir, ffmpeg string, s *Store) *Cache {
	_ = os.MkdirAll(dir, 0755)
	c := &Cache{dir: dir, ffmpeg: ffmpeg, store: s, entries: map[string]*cacheEntry{}, locks: map[string]*sync.Mutex{}, jobs: map[string]bool{}}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".part") {
			_ = os.Remove(filepath.Join(dir, f.Name()))
			continue
		}
		if info, e := f.Info(); e == nil && !f.IsDir() {
			c.entries[f.Name()] = &cacheEntry{Path: filepath.Join(dir, f.Name()), Size: info.Size(), Used: info.ModTime()}
		}
	}
	return c
}
func (c *Cache) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used int64
	pending, active := 0, 0
	for _, wait := range c.jobs {
		active++
		if wait {
			pending++
		}
	}
	for _, e := range c.entries {
		used += e.Size
		active += e.Active
		if e.Pending {
			pending++
		}
	}
	return map[string]any{"used": used, "limit": c.store.Read().CacheLimit, "pending": pending, "active": active}
}
func (c *Cache) trimLocked(limit int64) {
	var used int64
	keys := []string{}
	for k, e := range c.entries {
		used += e.Size
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return c.entries[keys[i]].Used.Before(c.entries[keys[j]].Used) })
	for _, k := range keys {
		e := c.entries[k]
		if e.Pending || used > limit {
			if e.Active > 0 {
				if e.Pending {
					continue
				}
				continue
			}
			if os.Remove(e.Path) == nil {
				used -= e.Size
				delete(c.entries, k)
			}
		}
	}
}
func (c *Cache) Trim() {
	limit := c.store.Read().CacheLimit
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trimLocked(limit)
}
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.Pending = true
	}
	for id := range c.jobs {
		c.jobs[id] = true
	}
	c.trimLocked(0)
}
func (c *Cache) Acquire(ctx context.Context, t Track, input string, rule Conversion, gain *float64) (string, func(), error) {
	data, _ := json.Marshal([]any{t.ID, t.Modified, t.Size, t.Revision, rule.Codec, rule.OutputBitrate, rule.OutputSampleRate, gain, rule.GainIdentity})
	hash := sha256.Sum256(data)
	key := hex.EncodeToString(hash[:]) + "." + rule.Codec
	c.mu.Lock()
	lock := c.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[key] = lock
	}
	c.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	if e := c.entries[key]; e != nil && !e.Pending {
		e.Active++
		e.Used = time.Now()
		p := e.Path
		c.mu.Unlock()
		return p, c.release(key), nil
	}
	jobID := newID()
	c.jobs[jobID] = false
	c.mu.Unlock()
	retainJob := false
	defer func() {
		if !retainJob {
			c.mu.Lock()
			delete(c.jobs, jobID)
			c.mu.Unlock()
		}
	}()
	temporaryRelease := func(p string) func() {
		return func() { _ = os.Remove(p); c.mu.Lock(); delete(c.jobs, jobID); c.mu.Unlock() }
	}
	temp, e := os.CreateTemp(c.dir, "transcode-*.part")
	if e != nil {
		return "", nil, e
	}
	p := temp.Name()
	temp.Close()
	args := []string{"-v", "error", "-nostdin", "-i", input, "-map", "0:a:0", "-vn"}
	codec := map[string]string{"mp3": "libmp3lame", "opus": "libopus", "flac": "flac", "aac": "aac", "wav": "pcm_s16le"}[rule.Codec]
	format := map[string]string{"mp3": "mp3", "opus": "ogg", "flac": "flac", "aac": "adts", "wav": "wav"}[rule.Codec]
	args = append(args, "-c:a", codec)
	if rule.OutputBitrate > 0 && rule.Codec != "flac" && rule.Codec != "wav" {
		args = append(args, "-b:a", fmt.Sprintf("%dk", rule.OutputBitrate))
	}
	if rule.OutputSampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(rule.OutputSampleRate))
	}
	if gain != nil {
		args = append(args, "-af", fmt.Sprintf("volume=%.10f", *gain))
	}
	args = append(args, "-f", format, "-y", p)
	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	if out, e := cmd.CombinedOutput(); e != nil {
		_ = os.Remove(p)
		return "", nil, fmt.Errorf("conversion failed: %s (%w)", strings.TrimSpace(string(out)), e)
	}
	info, e := os.Stat(p)
	if e != nil {
		_ = os.Remove(p)
		return "", nil, e
	}
	limit := c.store.Read().CacheLimit
	c.mu.Lock()
	c.trimLocked(max(0, limit-info.Size()))
	var used int64
	for _, e := range c.entries {
		used += e.Size
	}
	if used+info.Size() > limit || c.jobs[jobID] {
		retainJob = true
		c.mu.Unlock()
		return p, temporaryRelease(p), nil
	}
	final := filepath.Join(c.dir, key)
	if existing := c.entries[key]; existing != nil && existing.Active > 0 {
		retainJob = true
		c.mu.Unlock()
		return p, temporaryRelease(p), nil
	}
	_ = os.Remove(final)
	if e = os.Rename(p, final); e != nil {
		c.mu.Unlock()
		_ = os.Remove(p)
		return "", nil, e
	}
	c.entries[key] = &cacheEntry{Path: final, Size: info.Size(), Used: time.Now(), Active: 1}
	c.mu.Unlock()
	return final, c.release(key), nil
}
func (c *Cache) release(key string) func() {
	return func() {
		c.mu.Lock()
		if e := c.entries[key]; e != nil {
			e.Active--
			e.Used = time.Now()
			_ = os.Chtimes(e.Path, e.Used, e.Used)
		}
		c.mu.Unlock()
		c.Trim()
	}
}
func matched(t Track, sets []RuleSet, id string) *Conversion {
	for _, set := range sets {
		if set.ID == id {
			for _, r := range set.Rules {
				formats := r.Formats
				if formats == nil && r.Format != "" {
					formats = []string{r.Format}
				}
				matchesFormat := len(formats) == 0
				for _, format := range formats {
					if strings.EqualFold(format, t.Format) {
						matchesFormat = true
						break
					}
				}
				if !matchesFormat {
					continue
				}
				if r.BitrateOp != "" && !compare(float64(t.Bitrate), r.Bitrate, r.BitrateOp) {
					continue
				}
				if r.SampleRateOp != "" && !compare(float64(t.SampleRate), r.SampleRate, r.SampleRateOp) {
					continue
				}
				return &r
			}
			break
		}
	}
	return nil
}
func (a *App) stream(w http.ResponseWriter, r *http.Request) {
	t, p, e := a.track(r.PathValue("id"))
	if e != nil {
		respond(w, 404, map[string]string{"error": e.Error()})
		return
	}
	rule := matched(t, a.store.Read().RuleSets, r.URL.Query().Get("ruleSet"))
	var gain *float64
	if r.URL.Query().Get("output") == "airplay" {
		preamp, _ := numeric(r.URL.Query().Get("preamp"))
		g := ReplayGain(t, r.URL.Query().Get("gain"), math.Max(-30, math.Min(30, preamp)), r.URL.Query().Get("protect") != "false")
		if rule == nil && g != 1 {
			rule = &Conversion{Codec: "wav", OutputSampleRate: 44100}
		}
		if rule != nil {
			gain = &g
			rule.GainIdentity = r.URL.Query().Get("gain") + ":" + r.URL.Query().Get("preamp") + ":" + r.URL.Query().Get("protect")
		}
	}
	if rule == nil {
		// Do not rely on OS MIME registrations or sniffing for media requests.
		if contentType := audioContentType(filepath.Ext(p)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
	}
	if rule != nil {
		var release func()
		p, release, e = a.cache.Acquire(r.Context(), t, p, *rule, gain)
		if e != nil {
			respond(w, 422, map[string]string{"error": e.Error()})
			return
		}
		defer release()
		mime := map[string]string{"mp3": "audio/mpeg", "opus": "audio/ogg", "flac": "audio/flac", "aac": "audio/aac", "wav": "audio/wav"}[rule.Codec]
		w.Header().Set("Content-Type", mime)
	}
	http.ServeFile(w, r, p)
}

func audioContentType(extension string) string {
	return map[string]string{
		".mp3": "audio/mpeg", ".flac": "audio/flac", ".m4a": "audio/mp4",
		".mp4": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
		".ogg": "audio/ogg", ".opus": "audio/ogg", ".aif": "audio/aiff", ".aiff": "audio/aiff",
	}[strings.ToLower(extension)]
}
func ReplayGain(t Track, mode string, preamp float64, protect bool) float64 {
	if mode == "off" || mode == "" {
		return 1
	}
	gain, peak := t.TrackGain, t.TrackPeak
	if mode == "album" && t.AlbumGain != nil {
		gain, peak = t.AlbumGain, t.AlbumPeak
	}
	if gain == nil {
		return 1
	}
	out := math.Pow(10, (*gain+preamp)/20)
	if protect && peak != nil && *peak > 0 {
		out = math.Min(out, 1 / *peak)
	}
	return out
}
func (a *App) capabilities(w http.ResponseWriter, r *http.Request) {
	formats := []string{}
	seen := map[string]bool{}
	for _, t := range a.store.Read().Tracks {
		if !t.Missing && !seen[t.Format] {
			seen[t.Format] = true
			formats = append(formats, t.Format)
		}
	}
	sort.Strings(formats)
	_, probeErr := exec.LookPath(a.config.FFprobe)
	encoders := a.encoders()
	respond(w, 200, map[string]any{"formats": formats, "outputs": encoders, "bitrates": []int{64, 96, 128, 160, 192, 256, 320}, "sampleRates": []int{22050, 44100, 48000}, "ffprobe": probeErr == nil, "airplay": a.config.OwnTone != "", "gapless": "Web Audio decoded playback supports sample-accurate transitions; streamed media and AirPlay depend on the decoder/output."})
}
func (a *App) encoders() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, a.config.FFmpeg, "-hide_banner", "-encoders").Output()
	result := []string{}
	if e != nil {
		return result
	}
	for _, pair := range [][2]string{{"mp3", "libmp3lame"}, {"opus", "libopus"}, {"flac", "flac"}, {"aac", "aac"}, {"wav", "pcm_s16le"}} {
		if strings.Contains(string(out), " "+pair[1]+" ") {
			result = append(result, pair[0])
		}
	}
	return result
}
func (a *App) ruleSets(w http.ResponseWriter, r *http.Request) {
	var set RuleSet
	if r.Method != "DELETE" {
		if !decode(w, r, &set) {
			return
		}
		if strings.TrimSpace(set.Name) == "" {
			problem(w, errors.New("name required"))
			return
		}
		encoders := "|" + strings.Join(a.encoders(), "|") + "|"
		for _, rule := range set.Rules {
			if !strings.Contains(encoders, "|"+rule.Codec+"|") || rule.Codec == "" {
				problem(w, errors.New("unsupported encoder"))
				return
			}
			if rule.OutputSampleRate != 0 && rule.OutputSampleRate != 22050 && rule.OutputSampleRate != 44100 && rule.OutputSampleRate != 48000 {
				problem(w, errors.New("invalid sample rate"))
				return
			}
			if rule.Codec == "opus" && rule.OutputSampleRate != 0 && rule.OutputSampleRate != 48000 {
				problem(w, errors.New("Opus requires 48000 Hz output"))
				return
			}
			if rule.Codec != "wav" && rule.Codec != "flac" && (rule.OutputBitrate < 64 || rule.OutputBitrate > 320) {
				problem(w, errors.New("bitrate must be 64–320 kbps"))
				return
			}
			for _, op := range []string{rule.BitrateOp, rule.SampleRateOp} {
				if op != "" && !strings.Contains("|eq|gt|gte|lt|lte|", "|"+op+"|") {
					problem(w, errors.New("invalid comparison"))
					return
				}
			}
		}
	}
	e := a.store.Update(func(st *State) error {
		if r.Method == "POST" {
			set.ID = newID()
			st.RuleSets = append(st.RuleSets, set)
			return nil
		}
		for i, s := range st.RuleSets {
			if s.ID == r.PathValue("id") {
				if r.Method == "DELETE" {
					st.RuleSets = append(st.RuleSets[:i], st.RuleSets[i+1:]...)
				} else {
					set.ID = s.ID
					st.RuleSets[i] = set
				}
				return nil
			}
		}
		return errors.New("rule set not found")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, set)
}
