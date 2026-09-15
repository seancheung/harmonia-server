package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ScanStatus struct {
	Running    bool     `json:"running"`
	Processed  int      `json:"processed"`
	Total      int      `json:"total"`
	Errors     []string `json:"errors"`
	FinishedAt int64    `json:"finishedAt"`
}
type Scanner struct {
	mu      sync.Mutex
	status  ScanStatus
	store   *Store
	ffprobe string
	ffmpeg  string
	artDir  string
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

func globMatch(pattern, p string) bool {
	pattern = strings.Trim(strings.ReplaceAll(pattern, "\\", "/"), "/")
	p = strings.Trim(p, "/")
	if runtime.GOOS == "windows" {
		pattern = strings.ToLower(pattern)
		p = strings.ToLower(p)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	r, e := regexp.Compile(b.String())
	if e != nil {
		return false
	}
	for {
		if r.MatchString(p) {
			return true
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return false
}
func allowed(s Source, p string) bool {
	for _, r := range s.Ignore {
		if globMatch(r, p) {
			return false
		}
	}
	if len(s.Keep) == 0 {
		return true
	}
	for _, r := range s.Keep {
		if globMatch(r, p) {
			return true
		}
	}
	return false
}
func music(p string) bool {
	return strings.Contains("|mp3|flac|m4a|aac|ogg|opus|wav|aiff|aif|alac|wma|ape|wv|dsf|dff|", "|"+strings.TrimPrefix(strings.ToLower(filepath.Ext(p)), ".")+"|")
}
func (s *Scanner) Status() ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.status
	out.Errors = append([]string{}, out.Errors...)
	return out
}
func (s *Scanner) Start(force bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running {
		return false
	}
	s.status = ScanStatus{Running: true, Errors: []string{}}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.run(ctx, force) }()
	return true
}
func (s *Scanner) problem(e error) {
	s.mu.Lock()
	s.status.Errors = append(s.status.Errors, e.Error())
	s.mu.Unlock()
}
func (s *Scanner) run(ctx context.Context, force bool) {
	defer func() {
		s.mu.Lock()
		s.status.Running = false
		s.status.FinishedAt = time.Now().UnixMilli()
		s.mu.Unlock()
	}()
	snapshot := s.store.Read()
	for _, source := range snapshot.Sources {
		if ctx.Err() != nil {
			return
		}
		files := []string{}
		folderTimes := map[string]FileTimes{}
		walkRoot := source.Path
		if resolved, e := filepath.EvalSymlinks(source.Path); e == nil {
			walkRoot = resolved
		}
		err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if p == walkRoot && !d.IsDir() {
				return fmt.Errorf("source is not a directory")
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if info, err := d.Info(); err == nil {
					rel, _ := filepath.Rel(walkRoot, p)
					if rel == "." {
						rel = ""
					}
					folderTimes[filepath.ToSlash(rel)] = FileTimes{ModifiedAt: info.ModTime().UnixMilli(), CreatedAt: creationTime(p, info)}
				}
			}
			if !d.IsDir() && music(p) {
				rel, e := filepath.Rel(walkRoot, p)
				if e != nil {
					return e
				}
				if allowed(source, filepath.ToSlash(rel)) {
					files = append(files, p)
				}
			}
			return ctx.Err()
		})
		if err != nil {
			s.problem(fmt.Errorf("%s: %w", source.Name, err))
			_ = s.store.Update(func(st *State) error {
				for i := range st.Sources {
					if st.Sources[i].ID == source.ID {
						st.Sources[i].Error = err.Error()
					}
				}
				return nil
			})
			continue
		}
		s.mu.Lock()
		s.status.Total += len(files)
		s.mu.Unlock()
		old := map[string]Track{}
		for _, t := range snapshot.Tracks {
			if t.SourceID == source.ID && !t.Missing {
				old[t.Path] = t
			}
		}
		found := map[string]Track{}
		for _, p := range files {
			if ctx.Err() != nil {
				return
			}
			rel, _ := filepath.Rel(walkRoot, p)
			rel = filepath.ToSlash(rel)
			info, e := os.Stat(p)
			if e != nil {
				s.problem(e)
				if t, ok := old[rel]; ok {
					found[rel] = t
				}
				continue
			}
			t, exists := old[rel]
			if !exists {
				t = Track{ID: newID(), SourceID: source.ID, Path: rel, Filename: filepath.Base(p), Folder: folderOf(rel), AddedAt: time.Now().UnixMilli()}
			}
			if force || !exists || t.Modified != info.ModTime().UnixNano() || t.Size != info.Size() {
				meta, e := s.probe(ctx, p)
				if e != nil {
					s.problem(fmt.Errorf("%s: %w", rel, e))
					if exists {
						found[rel] = t
					}
					continue
				}
				meta.ID = t.ID
				meta.SourceID = t.SourceID
				meta.Path = rel
				meta.Filename = t.Filename
				meta.Folder = t.Folder
				meta.AddedAt = t.AddedAt
				meta.Favorite = t.Favorite
				meta.PlayCount = t.PlayCount
				meta.LastPlayed = t.LastPlayed
				meta.Modified = info.ModTime().UnixNano()
				meta.Size = info.Size()
				meta.Revision = newID()
				meta.AlbumID = albumKey(meta)
				t = meta
			}
			t.Lyrics = localLyrics(p, t.Tags)
			t.ModifiedAt = info.ModTime().UnixMilli()
			t.CreatedAt = creationTime(p, info)
			t.Cover = ""
			found[rel] = t
			s.mu.Lock()
			s.status.Processed++
			s.mu.Unlock()
		}
		e := s.store.Update(func(st *State) error {
			present := false
			for i := range st.Sources {
				if st.Sources[i].ID == source.ID {
					st.Sources[i].Error = ""
					st.Sources[i].FolderTimes = folderTimes
					present = true
				}
			}
			if !present {
				return nil
			}
			for i := range st.Tracks {
				t := &st.Tracks[i]
				if t.SourceID != source.ID || t.Missing {
					continue
				}
				if fresh, ok := found[t.Path]; ok {
					fresh.Favorite = t.Favorite
					fresh.PlayCount = t.PlayCount
					fresh.LastPlayed = t.LastPlayed
					*t = fresh
					delete(found, t.Path)
				} else {
					t.Missing = true
					t.Favorite = false
				}
			}
			for _, t := range found {
				st.Tracks = append(st.Tracks, t)
			}
			return nil
		})
		if e != nil {
			s.problem(e)
		}
	}
	s.refreshCovers(ctx)
}

type probeResult struct {
	Format struct {
		Duration string            `json:"duration"`
		Bitrate  string            `json:"bit_rate"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType  string            `json:"codec_type"`
		CodecName  string            `json:"codec_name"`
		SampleRate string            `json:"sample_rate"`
		Bitrate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"streams"`
}

func (s *Scanner) probe(ctx context.Context, p string) (Track, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, s.ffprobe, "-v", "error", "-show_format", "-show_streams", "-of", "json", p).Output()
	if e != nil {
		return Track{}, fmt.Errorf("cannot read audio tags (ffprobe): %w", e)
	}
	var probe probeResult
	if e = json.Unmarshal(b, &probe); e != nil {
		return Track{}, e
	}
	tags := map[string]string{}
	for k, v := range probe.Format.Tags {
		tags[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	t := Track{Tags: tags}
	audio := false
	for _, stream := range probe.Streams {
		if stream.CodecType == "audio" {
			audio = true
			t.Format = stream.CodecName
			t.SampleRate, _ = strconv.Atoi(stream.SampleRate)
			t.Bitrate, _ = strconv.Atoi(stream.Bitrate)
			for k, v := range stream.Tags {
				tags[strings.ToLower(k)] = strings.TrimSpace(v)
			}
			break
		}
	}
	if !audio {
		return t, fmt.Errorf("no audio stream")
	}
	if t.Bitrate == 0 {
		t.Bitrate, _ = strconv.Atoi(probe.Format.Bitrate)
	}
	t.Bitrate /= 1000
	t.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	t.Title = tags["title"]
	t.Artist = tags["artist"]
	t.Album = tags["album"]
	t.AlbumArtist = tags["album_artist"]
	if t.AlbumArtist == "" {
		t.AlbumArtist = tags["albumartist"]
	}
	t.Genre = tags["genre"]
	year := tags["date"]
	if year == "" {
		year = tags["year"]
	}
	if len(year) >= 4 {
		t.Year, _ = strconv.Atoi(year[:4])
	}
	t.Disc = parseIndex(tags["disc"])
	t.Number = parseIndex(tags["track"])
	t.TrackGain = tagNumber(tags["replaygain_track_gain"])
	t.AlbumGain = tagNumber(tags["replaygain_album_gain"])
	t.TrackPeak = tagNumber(tags["replaygain_track_peak"])
	t.AlbumPeak = tagNumber(tags["replaygain_album_peak"])
	return t, nil
}
func parseIndex(s string) int { n, _ := strconv.Atoi(strings.Split(s, "/")[0]); return n }
func tagNumber(s string) *float64 {
	p := strings.Fields(s)
	if len(p) == 0 {
		return nil
	}
	if n, ok := numeric(p[0]); ok {
		return &n
	}
	return nil
}
func localLyrics(p string, tags map[string]string) string {
	stem := strings.TrimSuffix(p, filepath.Ext(p))
	for _, ext := range []string{".lrc", ".txt"} {
		if b, e := os.ReadFile(stem + ext); e == nil && len(b) <= 2*1024*1024 {
			if value := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")); value != "" {
				return value
			}
		}
	}
	return embeddedLyrics(tags)
}

func embeddedLyrics(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, base := range []string{"lyrics", "unsyncedlyrics", "unsynced lyrics", "unsynced_lyrics", "uslt"} {
		for _, suffixed := range []bool{false, true} {
			for _, key := range keys {
				normalized := strings.ToLower(strings.TrimSpace(key))
				matches := normalized == base
				if suffixed {
					matches = strings.HasPrefix(normalized, base+"-") || strings.HasPrefix(normalized, base+":")
				}
				if matches {
					if value := strings.TrimSpace(strings.TrimPrefix(tags[key], "\ufeff")); value != "" {
						return value
					}
				}
			}
		}
	}
	return ""
}
func (s *Scanner) refreshCovers(ctx context.Context) {
	st := s.store.Read()
	sources := map[string]string{}
	for _, src := range st.Sources {
		sources[src.ID] = src.Path
	}
	albums := map[string][]Track{}
	for _, t := range st.Tracks {
		if !t.Missing {
			albums[t.AlbumID] = append(albums[t.AlbumID], t)
		}
	}
	covers := map[string]string{}
	artworkRevisions := map[string]string{}
	for id, tracks := range albums {
		sort.Slice(tracks, func(i, j int) bool {
			a, b := tracks[i], tracks[j]
			if a.Disc != b.Disc {
				if a.Disc == 0 {
					return false
				}
				if b.Disc == 0 {
					return true
				}
				return a.Disc < b.Disc
			}
			if a.Number != b.Number {
				if a.Number == 0 {
					return false
				}
				if b.Number == 0 {
					return true
				}
				return a.Number < b.Number
			}
			return a.ID < b.ID
		})
		checked := map[string]bool{}
		cover := ""
		for _, t := range tracks {
			dir := filepath.Join(sources[t.SourceID], filepath.FromSlash(t.Folder))
			if checked[dir] {
				continue
			}
			checked[dir] = true
			for _, name := range []string{"cover", "folder", "front"} {
				for _, ext := range []string{"jpg", "jpeg", "png"} {
					p := filepath.Join(dir, name+"."+ext)
					if cover != "" {
						break
					}
					if info, e := os.Stat(p); e == nil && info.Size() < 25*1024*1024 {
						if f, e := os.Open(p); e == nil {
							_, _, valid := image.DecodeConfig(f)
							f.Close()
							if valid == nil {
								cover = p
							}
						}
					}
				}
			}
			if cover != "" {
				break
			}
		}
		if cover == "" {
			for _, t := range tracks {
				p := filepath.Join(s.artDir, t.ID+"-"+t.Revision+".jpg")
				if validArtwork(p) {
					cover = p
					break
				}
				if s.extractArtwork(ctx, filepath.Join(sources[t.SourceID], filepath.FromSlash(t.Path)), p) {
					cover = p
					break
				}
				_ = os.Remove(p)
			}
		}
		covers[id] = cover
		if cover != "" {
			if b, e := os.ReadFile(cover); e == nil {
				hash := sha256.Sum256(b)
				artworkRevisions[id] = hex.EncodeToString(hash[:])
			}
		}
	}
	if e := s.store.Update(func(st *State) error {
		for i := range st.Tracks {
			if !st.Tracks[i].Missing {
				st.Tracks[i].Cover = covers[st.Tracks[i].AlbumID]
				st.Tracks[i].HasCover = covers[st.Tracks[i].AlbumID] != ""
				st.Tracks[i].ArtworkRevision = artworkRevisions[st.Tracks[i].AlbumID]
			}
		}
		return nil
	}); e != nil {
		s.problem(e)
	}
}

func validArtwork(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 || info.Size() > 25*1024*1024 {
		return false
	}
	_, _, err = image.DecodeConfig(f)
	return err == nil
}

func (s *Scanner) extractArtwork(ctx context.Context, input, output string) bool {
	// Copy embedded JPEG/PNG bytes first, without requiring an image encoder.
	// The cover handler detects the content type from the bytes, including PNGs.
	for _, codec := range []string{"copy", "mjpeg"} {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := exec.CommandContext(c, s.ffmpeg, "-v", "error", "-i", input,
			"-map", "0:v:0", "-an", "-frames:v", "1", "-c:v", codec,
			"-f", "image2", "-update", "1", "-y", output).Run()
		cancel()
		if err == nil && validArtwork(output) {
			return true
		}
		_ = os.Remove(output)
	}
	return false
}
