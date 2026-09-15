package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct{ Listen, DataDir, CacheDir, FFprobe, FFmpeg, Origin, Token, OwnTone, PublicURL string }

func env(k, v string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return v
}
func ConfigFromEnv() Config {
	return Config{Listen: env("HARMONIA_LISTEN", ":8090"), DataDir: env("HARMONIA_DATA", "./data"), CacheDir: os.Getenv("HARMONIA_CACHE"), FFprobe: env("HARMONIA_FFPROBE", "ffprobe"), FFmpeg: env("HARMONIA_FFMPEG", "ffmpeg"), Origin: env("HARMONIA_ORIGIN", "http://localhost:5173,http://127.0.0.1:5173"), Token: os.Getenv("HARMONIA_TOKEN"), OwnTone: os.Getenv("HARMONIA_OWNTONE"), PublicURL: env("HARMONIA_PUBLIC_URL", "http://localhost:8090")}
}

type App struct {
	config       Config
	store        *Store
	scanner      *Scanner
	cache        *Cache
	remote       *Remote
	waveformSlot chan struct{}
}

func New(c Config) (*App, error) {
	if c.CacheDir == "" {
		c.CacheDir = filepath.Join(c.DataDir, "cache")
	}
	if err := os.MkdirAll(c.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	s, e := OpenStore(filepath.Join(c.DataDir, "harmonia.sqlite"))
	if e != nil {
		return nil, e
	}
	art := filepath.Join(c.DataDir, "artwork")
	if e = os.MkdirAll(art, 0755); e != nil {
		s.Close()
		return nil, e
	}
	a := &App{config: c, store: s, waveformSlot: make(chan struct{}, 1)}
	a.scanner = &Scanner{store: s, ffprobe: c.FFprobe, ffmpeg: c.FFmpeg, artDir: art}
	a.cache = NewCache(c.CacheDir, c.FFmpeg, s)
	a.remote = NewRemote(a)
	return a, nil
}
func (a *App) Close() {
	a.scanner.mu.Lock()
	if a.scanner.cancel != nil {
		a.scanner.cancel()
	}
	a.scanner.mu.Unlock()
	a.scanner.wg.Wait()
	a.remote.Close()
	_ = a.store.Close()
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, e error) { respond(w, 400, map[string]string{"error": e.Error()}) }
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		problem(w, e)
		return false
	}
	return true
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"status": "ok", "version": "1.0.0"})
	})
	m.HandleFunc("GET /api/library", a.library)
	m.HandleFunc("POST /api/tracks/query", a.query)
	m.HandleFunc("POST /api/sources", a.sources)
	m.HandleFunc("PUT /api/sources/{id}", a.sources)
	m.HandleFunc("DELETE /api/sources/{id}", a.sources)
	m.HandleFunc("GET /api/scan", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, a.scanner.Status()) })
	m.HandleFunc("DELETE /api/scan", func(w http.ResponseWriter, r *http.Request) {
		a.scanner.Stop()
		respond(w, 202, a.scanner.Status())
	})
	m.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		if !a.scanner.Start(r.URL.Query().Get("force") == "true") {
			respond(w, 409, map[string]string{"error": "scan already running"})
			return
		}
		respond(w, 202, a.scanner.Status())
	})
	m.HandleFunc("PUT /api/tracks/{id}/favorite", a.favorite)
	m.HandleFunc("POST /api/tracks/{id}/played", a.played)
	m.HandleFunc("DELETE /api/recent", a.clearRecent)
	m.HandleFunc("POST /api/playlists", a.playlists)
	m.HandleFunc("PUT /api/playlists/{id}", a.playlists)
	m.HandleFunc("DELETE /api/playlists/{id}", a.playlists)
	m.HandleFunc("POST /api/playlists/{id}/items", a.playlistItems)
	m.HandleFunc("GET /api/tracks/{id}/stream", a.stream)
	m.HandleFunc("GET /api/tracks/{id}/cover", a.cover)
	m.HandleFunc("GET /api/tracks/{id}/waveform", a.waveform)
	m.HandleFunc("GET /api/tracks/{id}/lyrics", a.lyrics)
	m.HandleFunc("GET /api/cache", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, a.cache.Status()) })
	m.HandleFunc("DELETE /api/cache", func(w http.ResponseWriter, r *http.Request) { a.cache.Clear(); respond(w, 200, a.cache.Status()) })
	m.HandleFunc("PUT /api/cache", a.cacheSettings)
	m.HandleFunc("PUT /api/tag-settings", a.tagSettings)
	m.HandleFunc("POST /api/rule-sets", a.ruleSets)
	m.HandleFunc("PUT /api/rule-sets/{id}", a.ruleSets)
	m.HandleFunc("DELETE /api/rule-sets/{id}", a.ruleSets)
	m.HandleFunc("GET /api/capabilities", a.capabilities)
	m.HandleFunc("GET /api/outputs", a.remote.Outputs)
	m.HandleFunc("GET /api/remote", a.remote.Status)
	m.HandleFunc("POST /api/remote", a.remote.Command)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("request panic: %v", v)
				respond(w, 500, map[string]string{"error": "internal server error"})
			}
		}()
		origin := r.Header.Get("Origin")
		if origin != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			// Same-origin media requests may include Origin, including through
			// a development proxy that preserves the browser-facing Host.
			valid := origin == scheme+"://"+r.Host
			for _, o := range strings.Split(a.config.Origin, ",") {
				if origin == strings.TrimSpace(o) {
					valid = true
				}
			}
			if !valid {
				respond(w, 403, map[string]string{"error": "origin not allowed"})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Range")
			w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Range, Content-Length")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if a.config.Token != "" && token != a.config.Token && r.URL.Path != "/api/health" {
			respond(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) library(w http.ResponseWriter, r *http.Request) {
	st := a.store.Read()
	st.Sessions = nil
	for i := range st.Tracks {
		st.Tracks[i].Cover = ""
		st.Tracks[i].Lyrics = ""
	}
	respond(w, 200, st)
}

type Query struct {
	Search     string `json:"search"`
	Rule       *Rule  `json:"rule"`
	Sort       string `json:"sort"`
	Desc       bool   `json:"desc"`
	Page       int    `json:"page"`
	PageSize   int    `json:"pageSize"`
	PlaylistID string `json:"playlistId"`
	All        bool   `json:"all"`
}

func queryTracks(st State, q Query) ([]Track, error) {
	tracks := []Track{}
	if q.Rule != nil {
		if e := q.Rule.Validate(); e != nil {
			return nil, e
		}
	}
	manual := false
	if q.PlaylistID != "" {
		found := false
		for _, p := range st.Playlists {
			if p.ID == q.PlaylistID {
				found = true
				if p.Smart {
					q.Rule = p.Rule
					q.Sort = p.Sort
					q.Desc = p.Desc
				} else {
					manual = true
					byID := map[string]Track{}
					for _, t := range st.Tracks {
						byID[t.ID] = t
					}
					for _, id := range p.Tracks {
						if t, ok := byID[id]; ok {
							tracks = append(tracks, t)
						}
					}
				}
				break
			}
		}
		if !found {
			return nil, errors.New("playlist not found")
		}
	}
	if !manual {
		for _, t := range st.Tracks {
			if !t.Missing {
				tracks = append(tracks, t)
			}
		}
	}
	out := []Track{}
	for _, t := range tracks {
		if q.Search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{t.Title, t.Artist, t.Album, t.Genre}, " ")), strings.ToLower(q.Search)) {
			continue
		}
		if q.Rule != nil && !q.Rule.Match(t) {
			continue
		}
		out = append(out, t)
	}
	if !manual {
		sortTracks(out, q.Sort, q.Desc)
	}
	return out, nil
}
func (a *App) query(w http.ResponseWriter, r *http.Request) {
	var q Query
	if !decode(w, r, &q) {
		return
	}
	ts, e := queryTracks(a.store.Read(), q)
	if e != nil {
		problem(w, e)
		return
	}
	total := len(ts)
	if !q.All {
		if q.Page < 1 {
			q.Page = 1
		}
		if q.PageSize != 25 && q.PageSize != 100 {
			q.PageSize = 50
		}
		start := min((q.Page-1)*q.PageSize, total)
		ts = ts[start:min(start+q.PageSize, total)]
	}
	respond(w, 200, map[string]any{"items": ts, "total": total, "page": q.Page, "pageSize": q.PageSize})
}
func (a *App) sources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var src Source
	if r.Method != "DELETE" {
		if !decode(w, r, &src) {
			return
		}
		src.Name = strings.TrimSpace(src.Name)
		p, e := filepath.Abs(src.Path)
		if e != nil || src.Name == "" || !filepath.IsAbs(src.Path) {
			problem(w, errors.New("name and absolute server path required"))
			return
		}
		src.Path = filepath.Clean(p)
		src.Keep = cleanRules(src.Keep)
		src.Ignore = cleanRules(src.Ignore)
		for _, rules := range [][]string{src.Keep, src.Ignore} {
			for _, rule := range rules {
				if strings.Contains(rule, "..") || strings.HasPrefix(rule, "/") || strings.Contains(rule, ":") {
					problem(w, errors.New("rules must be relative paths"))
					return
				}
			}
		}
	}
	e := a.store.Update(func(st *State) error {
		if r.Method != "DELETE" {
			for _, existing := range st.Sources {
				if existing.ID != id && strings.EqualFold(strings.TrimSpace(existing.Name), src.Name) {
					return errors.New("sourceNameExists")
				}
			}
		}
		if r.Method == "POST" {
			for _, s := range st.Sources {
				if s.Path == src.Path {
					return errors.New("source already exists")
				}
			}
			src.ID = newID()
			st.Sources = append(st.Sources, src)
			return nil
		}
		for i, s := range st.Sources {
			if s.ID == id {
				if r.Method == "DELETE" {
					st.Sources = append(st.Sources[:i], st.Sources[i+1:]...)
					for j := range st.Tracks {
						if st.Tracks[j].SourceID == id {
							st.Tracks[j].Missing = true
							st.Tracks[j].Favorite = false
						}
					}
				} else {
					if src.Path != s.Path {
						return errors.New("remove and add a new source to change its root path")
					}
					src.ID = id
					src.Error = s.Error
					st.Sources[i] = src
				}
				return nil
			}
		}
		return errors.New("source not found")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, src)
}
func (a *App) favorite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Favorite bool `json:"favorite"`
	}
	if !decode(w, r, &body) {
		return
	}
	e := a.store.Update(func(st *State) error {
		for i := range st.Tracks {
			if st.Tracks[i].ID == r.PathValue("id") && !st.Tracks[i].Missing {
				st.Tracks[i].Favorite = body.Favorite
				return nil
			}
		}
		return errors.New("track unavailable")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, body)
}
func (a *App) played(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Session   string  `json:"session"`
		Seconds   float64 `json:"seconds"`
		Completed bool    `json:"completed"`
		Seeked    bool    `json:"seeked"`
	}
	if !decode(w, r, &b) {
		return
	}
	if len(b.Session) < 16 || len(b.Session) > 100 || b.Seconds < 0 {
		problem(w, errors.New("invalid playback session"))
		return
	}
	e := a.store.Update(func(st *State) error {
		if st.Sessions == nil {
			st.Sessions = map[string]bool{}
		}
		key := r.PathValue("id") + ":" + b.Session
		if st.Sessions[key] {
			return nil
		}
		for i := range st.Tracks {
			t := &st.Tracks[i]
			if t.ID == r.PathValue("id") && !t.Missing {
				valid := b.Seconds >= 15 || (t.Duration > 0 && t.Duration < 15 && b.Completed && !b.Seeked && b.Seconds >= t.Duration-0.25)
				if !valid {
					return nil
				}
				t.PlayCount++
				t.LastPlayed = time.Now().UnixMilli()
				st.Sessions[key] = true
				trimRecent(st)
				return nil
			}
		}
		return errors.New("track unavailable")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func trimRecent(st *State) {
	ts := append([]Track{}, st.Tracks...)
	sortTracks(ts, "lastPlayed", true)
	if len(ts) > 500 {
		remove := map[string]bool{}
		for _, t := range ts[500:] {
			remove[t.ID] = true
		}
		for i := range st.Tracks {
			if remove[st.Tracks[i].ID] {
				st.Tracks[i].LastPlayed = 0
			}
		}
	}
}
func (a *App) clearRecent(w http.ResponseWriter, r *http.Request) {
	e := a.store.Update(func(st *State) error {
		for i := range st.Tracks {
			st.Tracks[i].LastPlayed = 0
		}
		return nil
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) playlists(w http.ResponseWriter, r *http.Request) {
	var p Playlist
	if r.Method != "DELETE" {
		if !decode(w, r, &p) {
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			problem(w, errors.New("playlist name required"))
			return
		}
		if p.Smart {
			if p.Rule == nil {
				problem(w, errors.New("smart playlist needs rules"))
				return
			}
			if e := p.Rule.Validate(); e != nil {
				problem(w, e)
				return
			}
			if p.Sort == "" {
				p.Sort = "addedAt"
				p.Desc = true
			}
		}
	}
	e := a.store.Update(func(st *State) error {
		if r.Method == "POST" {
			p.ID = newID()
			p.Tracks = []string{}
			st.Playlists = append(st.Playlists, p)
			return nil
		}
		for i, old := range st.Playlists {
			if old.ID == r.PathValue("id") {
				if r.Method == "DELETE" {
					st.Playlists = append(st.Playlists[:i], st.Playlists[i+1:]...)
				} else {
					p.ID = old.ID
					p.Tracks = old.Tracks
					if old.Smart != p.Smart {
						return errors.New("playlist type cannot change")
					}
					st.Playlists[i] = p
				}
				return nil
			}
		}
		return errors.New("playlist not found")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, p)
}
func (a *App) playlistItems(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action   string   `json:"action"`
		IDs      []string `json:"ids"`
		Position int      `json:"position"`
	}
	if !decode(w, r, &b) {
		return
	}
	e := a.store.Update(func(st *State) error {
		for i := range st.Playlists {
			p := &st.Playlists[i]
			if p.ID != r.PathValue("id") {
				continue
			}
			if p.Smart {
				return errors.New("smart playlists cannot be manually edited")
			}
			tracks := map[string]Track{}
			for _, t := range st.Tracks {
				tracks[t.ID] = t
			}
			switch b.Action {
			case "add":
				seen := map[string]bool{}
				for _, id := range p.Tracks {
					seen[id] = true
				}
				for _, id := range b.IDs {
					if seen[id] {
						return fmt.Errorf("duplicate track: %s", id)
					}
					t, ok := tracks[id]
					if !ok || t.Missing {
						return fmt.Errorf("track unavailable: %s", id)
					}
					seen[id] = true
				}
				p.Tracks = append(p.Tracks, b.IDs...)
			case "remove", "clean":
				out := []string{}
				for _, id := range p.Tracks {
					remove := false
					if b.Action == "clean" {
						t, ok := tracks[id]
						remove = !ok || t.Missing
					} else {
						for _, v := range b.IDs {
							if id == v {
								remove = true
							}
						}
					}
					if !remove {
						out = append(out, id)
					}
				}
				p.Tracks = out
			case "move":
				if len(b.IDs) != 1 || b.Position < 1 || b.Position > len(p.Tracks) {
					return errors.New("invalid full-list position")
				}
				from := -1
				for j, id := range p.Tracks {
					if id == b.IDs[0] {
						from = j
					}
				}
				if from < 0 {
					return errors.New("item not found")
				}
				id := p.Tracks[from]
				p.Tracks = append(p.Tracks[:from], p.Tracks[from+1:]...)
				pos := b.Position - 1
				p.Tracks = append(p.Tracks, "")
				copy(p.Tracks[pos+1:], p.Tracks[pos:])
				p.Tracks[pos] = id
			default:
				return errors.New("unknown action")
			}
			return nil
		}
		return errors.New("playlist not found")
	})
	if e != nil {
		problem(w, e)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) track(id string) (Track, string, error) {
	return trackFromState(a.store.Read(), id)
}

func trackFromState(st State, id string) (Track, string, error) {
	for _, t := range st.Tracks {
		if t.ID == id && !t.Missing {
			for _, s := range st.Sources {
				if s.ID == t.SourceID {
					p := filepath.Join(s.Path, filepath.FromSlash(t.Path))
					resolved, e := filepath.EvalSymlinks(p)
					if e != nil {
						return t, "", e
					}
					root, e := filepath.EvalSymlinks(s.Path)
					if e != nil {
						return t, "", e
					}
					rel, e := filepath.Rel(root, resolved)
					if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return t, "", errors.New("file outside source")
					}
					return t, resolved, nil
				}
			}
		}
	}
	return Track{}, "", errors.New("track unavailable")
}
func (a *App) cover(w http.ResponseWriter, r *http.Request) {
	t, _, e := a.track(r.PathValue("id"))
	if e != nil || t.Cover == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(t.Cover)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// Only a matching content revision is safe for long-lived browser caching.
	w.Header().Set("Cache-Control", "private, no-cache")
	if t.ArtworkRevision != "" {
		w.Header().Set("ETag", fmt.Sprintf("%q", t.ArtworkRevision))
		if r.URL.Query().Get("v") == t.ArtworkRevision {
			w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		}
	}
	// Cached artwork may contain PNG bytes despite a legacy .jpg filename.
	var header [512]byte
	n, _ := f.Read(header[:])
	w.Header().Set("Content-Type", http.DetectContentType(header[:n]))
	if _, err = f.Seek(0, 0); err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "artwork unavailable", 500)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
func (a *App) lyrics(w http.ResponseWriter, r *http.Request) {
	t, _, e := a.track(r.PathValue("id"))
	if e != nil {
		problem(w, e)
		return
	}
	lyrics := t.Lyrics
	if strings.TrimSpace(lyrics) == "" {
		lyrics = embeddedLyrics(t.Tags)
	}
	respond(w, 200, map[string]string{"lyrics": lyrics})
}
func (a *App) cacheSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Limit int64 `json:"limit"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Limit < 0 {
		problem(w, errors.New("cache limit must be nonnegative"))
		return
	}
	if e := a.store.Update(func(st *State) error { st.CacheLimit = b.Limit; return nil }); e != nil {
		problem(w, e)
		return
	}
	a.cache.Trim()
	respond(w, 200, a.cache.Status())
}
func intQuery(r *http.Request, key string) int {
	v, _ := strconv.Atoi(r.URL.Query().Get(key))
	return v
}

func cleanRules(rules []string) []string {
	out := []string{}
	for _, r := range rules {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
