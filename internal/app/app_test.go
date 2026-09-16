package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, e := New(Config{DataDir: t.TempDir(), FFprobe: "ffprobe", FFmpeg: "ffmpeg", Origin: "http://localhost:5173"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(a.Close)
	return a
}
func TestCacheDirectoryConfig(t *testing.T) {
	for _, custom := range []bool{false, true} {
		data := t.TempDir()
		cache := ""
		want := filepath.Join(data, "cache")
		if custom {
			cache = filepath.Join(t.TempDir(), "transcodes")
			want = cache
		}
		t.Setenv("HARMONIA_DATA", data)
		t.Setenv("HARMONIA_CACHE", cache)
		t.Setenv("HARMONIA_OWNTONE", "")
		a, err := New(ConfigFromEnv())
		if err != nil {
			t.Fatal(err)
		}
		if a.cache.dir != want {
			t.Errorf("cache directory = %q, want %q", a.cache.dir, want)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("cache directory was not created: %v", err)
		}
		a.Close()
	}
}
func request(t *testing.T, a *App, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	res := httptest.NewRecorder()
	a.Handler().ServeHTTP(res, req)
	return res
}
func TestPathRules(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{{"**/*.flac", "song.flac", true}, {"**/*.flac", "a/b/song.flac", true}, {"*.flac", "a/song.flac", false}, {"Jazz", "Jazz/Disc 1/song.flac", true}, {"Jazz", "Jazz Live/song.flac", false}, {"Jazz/**/song?.flac", "Jazz/song1.flac", true}, {"Jazz/**/song?.flac", "Jazz/a/b/song1.flac", true}, {"Jazz/*/song?.flac", "Jazz/a/b/song1.flac", false}}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("%q on %q = %v", c.pattern, c.path, got)
		}
	}
	s := Source{Keep: []string{"**/*.flac"}, Ignore: []string{"Extras/"}}
	if allowed(s, "Extras/a.flac") || !allowed(s, "Album/a.flac") {
		t.Fatal("ignore must override keep")
	}
}
func TestAlbumIdentity(t *testing.T) {
	a := Track{Album: "  Blue ", AlbumArtist: "One; Two", Year: 2020}
	b := Track{Album: "blue", AlbumArtist: " two ; ONE ", Year: 2020}
	if albumKey(a) != albumKey(b) {
		t.Fatal("normalized members should match")
	}
	b.Year = 0
	if albumKey(a) == albumKey(b) {
		t.Fatal("unknown year should not merge")
	}
	if albumKey(Track{}) != albumKey(Track{Album: " "}) {
		t.Fatal("missing album normalization")
	}
}

func TestAlbumArtistTagVariants(t *testing.T) {
	for _, key := range []string{"album_artist", "albumartist", "album artist", "album-artist"} {
		t.Run(key, func(t *testing.T) {
			artist := albumArtistTag(map[string]string{key: " Album Artist "})
			if artist != "Album Artist" {
				t.Fatalf("album artist = %q", artist)
			}
			a := Track{Album: "Flow", AlbumArtist: artist, Artist: "Singer A", Year: 2023}
			b := a
			b.Artist = "Singer B"
			if albumKey(a) != albumKey(b) {
				t.Fatal("track artists must not split an album with a shared album artist")
			}
		})
	}
	if got := albumArtistTag(map[string]string{"album_artist": " ", "album artist": "Fallback"}); got != "Fallback" {
		t.Fatalf("empty tag should allow fallback, got %q", got)
	}
}
func TestFilterAndSort(t *testing.T) {
	r := Rule{Mode: "any", Rules: []Rule{{Mode: "all", Rules: []Rule{{Field: "genre", Op: "contains", Value: "Jazz"}, {Field: "year", Op: "gte", Value: 2020}}}, {Field: "favorite", Op: "eq", Value: true}}}
	if e := r.Validate(); e != nil {
		t.Fatal(e)
	}
	if !r.Match(Track{Genre: "Jazz; Soul", Year: 2020}) || r.Match(Track{Genre: "Jazz", Year: 2019}) {
		t.Fatal("group boundary")
	}
	for _, tc := range []struct {
		op, value string
		want      bool
	}{
		{"contains", `jazz\01`, true}, {"notContains", "Rock", true},
		{"eq", "albums/jazz/01.flac", true}, {"ne", "Albums/Jazz", true},
		{"eq", "Albums/Jazz", false}, {"notContains", "01.flac", false},
	} {
		rule := Rule{Field: "path", Op: tc.op, Value: tc.value}
		if err := rule.Validate(); err != nil {
			t.Fatal(err)
		}
		if rule.Match(Track{Path: "Albums/Jazz/01.flac"}) != tc.want {
			t.Fatalf("path rule failed: %+v", tc)
		}
	}
	if (Rule{Mode: "all"}).Validate() == nil {
		t.Fatal("empty groups must fail")
	}
	invalid := Rule{Field: "year", Op: "gte", Value: "NaN"}
	if invalid.Validate() == nil {
		t.Fatal("invalid numeric value")
	}
	tracks := []Track{{ID: "a", Year: 0}, {ID: "b", Year: 2000, Artist: "Z"}, {ID: "c", Year: 2020}, {ID: "d", Year: 2000, Artist: "A"}}
	sortTracks(tracks, "year", true)
	if tracks[0].ID != "c" || tracks[1].ID != "d" || tracks[3].ID != "a" {
		t.Fatalf("unstable descending sort: %+v", tracks)
	}
	sortTracks(tracks, "year", false)
	if tracks[0].ID != "d" || tracks[3].ID != "a" {
		t.Fatal("missing must remain last")
	}
}
func TestPlaylistAtomicityAndMissing(t *testing.T) {
	a := testApp(t)
	_ = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source"}}
		st.Tracks = []Track{{ID: "a", SourceID: "source", Title: "A", Favorite: true}, {ID: "b", SourceID: "source", Title: "B"}}
		st.Playlists = []Playlist{{ID: "p", Name: "P", Tracks: []string{"a"}}}
		return nil
	})
	res := request(t, a, "POST", "/api/playlists/p/items", map[string]any{"action": "add", "ids": []string{"b", "a"}})
	if res.Code != 400 {
		t.Fatal(res.Code)
	}
	if len(a.store.Read().Playlists[0].Tracks) != 1 {
		t.Fatal("duplicate batch partially committed")
	}
	res = request(t, a, "POST", "/api/playlists/p/items", map[string]any{"action": "add", "ids": []string{"b"}})
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	res = request(t, a, "POST", "/api/playlists/p/items", map[string]any{"action": "move", "ids": []string{"b"}, "position": 1})
	if res.Code != 200 || a.store.Read().Playlists[0].Tracks[0] != "b" {
		t.Fatal("move failed")
	}
	res = request(t, a, "DELETE", "/api/sources/source", nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	st := a.store.Read()
	if !st.Tracks[0].Missing || st.Tracks[0].Favorite || len(st.Playlists[0].Tracks) != 2 {
		t.Fatal("source removal semantics")
	}
	request(t, a, "POST", "/api/playlists/p/items", map[string]any{"action": "clean", "ids": []string{}})
	if len(a.store.Read().Playlists[0].Tracks) != 0 {
		t.Fatal("missing cleanup")
	}
}
func TestHistoryDoesNotCountSeekOrDuplicate(t *testing.T) {
	a := testApp(t)
	_ = a.store.Update(func(st *State) error {
		st.Tracks = []Track{{ID: "short", Duration: 5}, {ID: "long", Duration: 60}}
		return nil
	})
	for _, payload := range []map[string]any{{"session": "1234567890123456", "seconds": 1, "completed": true, "seeked": true}, {"session": "1234567890123456", "seconds": 4, "completed": false, "seeked": false}} {
		request(t, a, "POST", "/api/tracks/short/played", payload)
	}
	if a.store.Read().Tracks[0].PlayCount != 0 {
		t.Fatal("seek counted")
	}
	payload := map[string]any{"session": "1234567890123456", "seconds": 5, "completed": true, "seeked": false}
	request(t, a, "POST", "/api/tracks/short/played", payload)
	request(t, a, "POST", "/api/tracks/short/played", payload)
	if a.store.Read().Tracks[0].PlayCount != 1 {
		t.Fatal("duplicate counted")
	}
	payload = map[string]any{"session": "1234567890123456", "seconds": 15, "completed": false, "seeked": true}
	request(t, a, "POST", "/api/tracks/long/played", payload)
	request(t, a, "DELETE", "/api/recent", nil)
	for _, track := range a.store.Read().Tracks {
		if track.PlayCount != 1 || track.LastPlayed != 0 {
			t.Fatal("history clearing reset counts")
		}
	}
}
func TestRecentLimitAndPersistence(t *testing.T) {
	a := testApp(t)
	e := a.store.Update(func(st *State) error {
		for i := 0; i < 510; i++ {
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), LastPlayed: int64(i + 1), PlayCount: 3})
		}
		trimRecent(st)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, track := range a.store.Read().Tracks {
		if track.LastPlayed > 0 {
			count++
		}
	}
	if count != 500 {
		t.Fatal(count)
	}
	other, e := OpenStore(filepath.Join(a.config.DataDir, "harmonia.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if len(other.Read().Tracks) != 510 {
		t.Fatal("state not persisted")
	}
}
func TestReplayGain(t *testing.T) {
	gain, peak, album := 6.0, 0.9, -3.0
	track := Track{TrackGain: &gain, TrackPeak: &peak}
	if ReplayGain(track, "off", 10, true) != 1 {
		t.Fatal("off gain")
	}
	if got := ReplayGain(track, "album", 0, true); got > 1/peak+0.00001 {
		t.Fatal("peak not protected")
	}
	track.AlbumGain = &album
	if ReplayGain(track, "album", 0, false) >= 1 {
		t.Fatal("album gain ignored")
	}
	if ReplayGain(Track{}, "track", 10, true) != 1 {
		t.Fatal("preamp applied without gain")
	}
}
func TestAuthAndPagination(t *testing.T) {
	a := testApp(t)
	a.config.Token = "secret"
	if request(t, a, "GET", "/api/library", nil).Code != 401 {
		t.Fatal("auth missing")
	}
	if request(t, a, "GET", "/api/library?token=secret", nil).Code != 200 {
		t.Fatal("token rejected")
	}
	a.config.Token = ""
	_ = a.store.Update(func(st *State) error {
		for i := 0; i < 75; i++ {
			st.Tracks = append(st.Tracks, Track{ID: fmt.Sprint(i), Title: fmt.Sprintf("Song %03d", i), Path: "hidden-keyword.mp3"})
		}
		return nil
	})
	res := request(t, a, "POST", "/api/tracks/query", Query{Page: 2, PageSize: 25, Sort: "title"})
	var result struct {
		Items []Track `json:"items"`
		Total int     `json:"total"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &result)
	if len(result.Items) != 25 || result.Total != 75 || result.Items[0].Title != "Song 025" {
		t.Fatal("pagination after sort failed")
	}
	ts, _ := queryTracks(a.store.Read(), Query{Search: "hidden-keyword"})
	if len(ts) != 0 {
		t.Fatal("search included path")
	}
}
func fixture(t *testing.T, path, title string) {
	t.Helper()
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg integration dependency is unavailable")
	}
	encoders, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if !strings.Contains(string(encoders), " flac ") {
		t.Skip("full FFmpeg encoder support is required; run these tests in the Docker test stage")
	}
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		t.Fatal(e)
	}
	wav := filepath.Join(t.TempDir(), "input.wav")
	var data bytes.Buffer
	data.WriteString("RIFF")
	_ = binary.Write(&data, binary.LittleEndian, uint32(36+88200))
	data.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(44100), uint32(88200), uint16(2), uint16(16)} {
		_ = binary.Write(&data, binary.LittleEndian, v)
	}
	data.WriteString("data")
	_ = binary.Write(&data, binary.LittleEndian, uint32(88200))
	data.Write(make([]byte, 88200))
	if e := os.WriteFile(wav, data.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", wav, "-metadata", "title="+title, "-metadata", "artist=Artist A; Artist B", "-metadata", "album=Test Album", "-metadata", "date=2024", "-metadata", "genre=Jazz; Soul", "-metadata", "REPLAYGAIN_TRACK_GAIN=-3.5 dB", "-y", path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%s: %v", out, e)
	}
}
func scanNow(t *testing.T, a *App, force bool) {
	t.Helper()
	a.scanner.run(context.Background(), force)
	if errors := a.scanner.Status().Errors; len(errors) > 0 {
		t.Fatal(errors)
	}
}
func TestScannerIntegration(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	p := filepath.Join(root, "Album", "one.flac")
	fixture(t, p, "First")
	_ = a.store.Update(func(st *State) error { st.Sources = []Source{{ID: "s", Name: "Music", Path: root}}; return nil })
	scanNow(t, a, false)
	initial := a.store.Read().Tracks[0]
	if initial.Title != "First" || initial.TrackGain == nil {
		t.Fatalf("tags: %+v", initial)
	}
	_ = a.store.Update(func(st *State) error {
		st.Tracks[0].Favorite = true
		st.Tracks[0].PlayCount = 9
		st.Playlists = []Playlist{{ID: "p", Tracks: []string{initial.ID}}}
		return nil
	})
	scanNow(t, a, false)
	if a.store.Read().Tracks[0].Revision != initial.Revision {
		t.Fatal("unchanged metadata reread")
	}
	fixture(t, p, "Other")
	scanNow(t, a, false)
	updated := a.store.Read().Tracks[0]
	if updated.Title != "Other" || updated.ID != initial.ID || !updated.Favorite || updated.PlayCount != 9 || updated.AddedAt != initial.AddedAt {
		t.Fatal("identity lost on tag edit")
	}
	revision := updated.Revision
	scanNow(t, a, true)
	if a.store.Read().Tracks[0].Revision == revision {
		t.Fatal("full read did not invalidate cache")
	}
	if e := os.Rename(p, filepath.Join(root, "Album", "renamed.flac")); e != nil {
		t.Fatal(e)
	}
	scanNow(t, a, false)
	st := a.store.Read()
	if len(st.Tracks) != 2 || !st.Tracks[0].Missing || st.Tracks[1].Favorite || st.Tracks[1].PlayCount != 0 {
		t.Fatal("rename inherited identity")
	}
	_ = a.store.Update(func(st *State) error {
		st.Sources[0].Path = filepath.Join(root, "offline")
		st.Tracks[1].Favorite = true
		return nil
	})
	a.scanner.run(context.Background(), false)
	st = a.store.Read()
	if st.Tracks[1].Missing || !st.Tracks[1].Favorite || st.Sources[0].Error == "" {
		t.Fatal("offline source removed tracks")
	}
}
func TestCacheIntegration(t *testing.T) {
	a := testApp(t)
	p := filepath.Join(t.TempDir(), "song.flac")
	fixture(t, p, "Cache")
	track := Track{ID: "t", Revision: "1", Size: 123}
	rule := Conversion{Codec: "mp3", OutputBitrate: 128, OutputSampleRate: 44100}
	path, release, e := a.cache.Acquire(context.Background(), track, p, rule, nil)
	if e != nil {
		t.Fatal(e)
	}
	a.cache.Clear()
	if _, e = os.Stat(path); e != nil {
		t.Fatal("active cache removed")
	}
	release()
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("pending cache not removed")
	}
	_ = a.store.Update(func(st *State) error { st.CacheLimit = 0; return nil })
	path, release, e = a.cache.Acquire(context.Background(), track, p, rule, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("zero limit stopped conversion")
	}
	release()
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("temporary output leaked")
	}
}
func TestSourceReadFailurePreservesCollection(t *testing.T) {
	a := testApp(t)
	_ = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Name: "Unavailable", Path: filepath.Join(t.TempDir(), "missing")}}
		st.Tracks = []Track{{ID: "t", SourceID: "s", Favorite: true, AddedAt: time.Now().UnixMilli()}}
		return nil
	})
	a.scanner.run(context.Background(), false)
	st := a.store.Read()
	if !st.Tracks[0].Favorite || st.Tracks[0].Missing || st.Sources[0].Error == "" {
		t.Fatal("unavailable source treated as deletion")
	}
}
func TestCORSBlocksUnapprovedMutation(t *testing.T) {
	a := testApp(t)
	r := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	r.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 || a.scanner.Status().Running {
		t.Fatal("unapproved origin mutated server")
	}
}

func TestCoverRefreshWithoutAudioChanges(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	audio := filepath.Join(root, "one.flac")
	fixture(t, audio, "Cover")
	_ = a.store.Update(func(st *State) error { st.Sources = []Source{{ID: "s", Name: "Music", Path: root}}; return nil })
	writeCover := func(name string, c color.RGBA) {
		f, e := os.Create(filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		img := image.NewRGBA(image.Rect(0, 0, 2, 2))
		img.Set(0, 0, c)
		if e = png.Encode(f, img); e != nil {
			t.Fatal(e)
		}
		f.Close()
	}
	writeCover("folder.png", color.RGBA{R: 255, A: 255})
	scanNow(t, a, false)
	first := a.store.Read().Tracks[0]
	if !first.HasCover || filepath.Base(first.Cover) != "folder.png" {
		t.Fatal("directory cover missing")
	}
	writeCover("cover.png", color.RGBA{B: 255, A: 255})
	scanNow(t, a, false)
	second := a.store.Read().Tracks[0]
	if second.Revision != first.Revision || second.ArtworkRevision == first.ArtworkRevision || filepath.Base(second.Cover) != "cover.png" {
		t.Fatal("cover update required an audio change")
	}
	if e := os.WriteFile(filepath.Join(root, "cover.jpg"), []byte("not an image"), 0600); e != nil {
		t.Fatal(e)
	}
	scanNow(t, a, false)
	if filepath.Base(a.store.Read().Tracks[0].Cover) != "cover.png" {
		t.Fatal("unreadable cover prevented fallback")
	}
	_ = os.Remove(filepath.Join(root, "cover.jpg"))
	_ = os.Remove(filepath.Join(root, "cover.png"))
	_ = os.Remove(filepath.Join(root, "folder.png"))
	scanNow(t, a, false)
	if a.store.Read().Tracks[0].HasCover {
		t.Fatal("deleted artwork remained")
	}
}
func TestOwnToneTimerPreservesQueueAndAvoidsPrematurePause(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/player" {
			respond(w, 200, map[string]any{"state": "play", "item_id": 2, "item_progress_ms": 5000, "item_length_ms": 10000})
			return
		}
		w.WriteHeader(204)
	}))
	defer own.Close()
	a, e := New(Config{DataDir: t.TempDir(), FFprobe: "ffprobe", FFmpeg: "ffmpeg", OwnTone: own.URL})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.remote.mu.Lock()
	a.remote.saved = RemoteSaved{Queue: []string{"a", "b", "c"}, ItemIDs: []float64{1, 2, 3}, Index: 1}
	a.remote.last = map[string]any{"state": "play", "item_id": float64(2)}
	a.remote.mu.Unlock()
	res := request(t, a, "POST", "/api/remote", map[string]any{"action": "timer", "deadline": 0, "finish": true})
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	a.remote.mu.Lock()
	if len(a.remote.saved.Queue) != 3 || !a.remote.saved.Trimmed {
		t.Fatal("timer discarded the saved queue")
	}
	a.remote.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	for _, call := range calls {
		if call == "PUT /api/player/pause" || call == "DELETE /api/queue/items/2" {
			t.Fatalf("timer interrupted current song: %s", call)
		}
	}
	if !slices.Contains(calls, "DELETE /api/queue/items/1") || !slices.Contains(calls, "DELETE /api/queue/items/3") {
		t.Fatal("following queue was not isolated")
	}
}

func TestEmbeddedArtworkCopyAndRepair(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	audio := filepath.Join(root, "audio.flac")
	fixture(t, audio, "Artwork")
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	picture := filepath.Join(t.TempDir(), "picture.png")
	if err := os.WriteFile(picture, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	embedded := filepath.Join(root, "embedded.flac")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-i", audio, "-i", picture, "-map", "0:a", "-map", "1:v", "-c", "copy", "-disposition:v", "attached_pic", embedded).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s: %v", out, err)
	}
	if err := os.Remove(audio); err != nil {
		t.Fatal(err)
	}
	_ = a.store.Update(func(st *State) error { st.Sources = []Source{{ID: "s", Path: root}}; return nil })
	scanNow(t, a, false)
	first := a.store.Read().Tracks[0]
	copied, err := os.ReadFile(first.Cover)
	if err != nil || !bytes.Equal(copied, original.Bytes()) {
		t.Fatal("embedded artwork was not copied losslessly", err)
	}
	response := request(t, a, "GET", "/api/tracks/"+first.ID+"/cover", nil)
	if response.Code != 200 || response.Header().Get("Content-Type") != "image/png" {
		t.Fatal("incorrect artwork response", response.Code, response.Header())
	}
	if err := os.WriteFile(first.Cover, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	scanNow(t, a, false)
	repaired := a.store.Read().Tracks[0]
	if !repaired.HasCover || !validArtwork(repaired.Cover) || repaired.Revision != first.Revision {
		t.Fatal("incremental scan failed to repair cached artwork")
	}
}

func TestLocalArtworkFixture(t *testing.T) {
	path := os.Getenv("HARMONIA_TEST_ARTWORK")
	if path == "" {
		t.Skip("optional local artwork fixture")
	}
	a := testApp(t)
	output := filepath.Join(t.TempDir(), "cover.jpg")
	if !a.scanner.extractArtwork(context.Background(), path, output) {
		t.Fatal("failed to extract local artwork")
	}
}

func TestTempoKeyAndNotContainsFilters(t *testing.T) {
	song := Track{Artist: "Artist One; Artist Two", Tags: map[string]string{"tbpm": "128.5", "initial_key": "F#m"}}
	for _, c := range []struct {
		rule Rule
		want bool
	}{
		{Rule{Field: "bpm", Op: "gt", Value: 120}, true},
		{Rule{Field: "key", Op: "eq", Value: "f#m"}, true},
		{Rule{Field: "artist", Op: "notContains", Value: "ONE"}, false},
		{Rule{Field: "key", Op: "notContains", Value: "8A"}, true},
	} {
		if err := c.rule.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.rule.Match(song) != c.want {
			t.Fatalf("unexpected match: %+v", c.rule)
		}
	}
	if (Rule{Field: "bpm", Op: "lt", Value: 120}).Match(Track{}) {
		t.Fatal("missing BPM matched a numeric range")
	}
	if !(Rule{Field: "key", Op: "notContains", Value: "8A"}).Match(Track{}) {
		t.Fatal("missing key should not contain text")
	}
	if (Rule{Field: "bpm", Op: "gte", Value: "invalid"}).Validate() == nil {
		t.Fatal("invalid BPM accepted")
	}
}

func TestFilesystemTimesRefreshWithoutMetadataProbe(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	path := filepath.Join(root, "track.mp3")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Path: root}}
		st.Tracks = []Track{{ID: "t", SourceID: "s", Path: "track.mp3", Modified: info.ModTime().UnixNano(), Size: info.Size(), Revision: "unchanged", TagVersion: nativeTagVersion}}
		return nil
	})
	a.scanner.run(context.Background(), false)
	st := a.store.Read()
	if st.Tracks[0].ModifiedAt != info.ModTime().UnixMilli() || st.Tracks[0].Revision != "unchanged" {
		t.Fatal("file timestamps were not refreshed incrementally")
	}
	if st.Sources[0].FolderTimes[""].ModifiedAt == 0 {
		t.Fatal("root folder timestamp missing")
	}
	if got := creationTime(path, info); st.Tracks[0].CreatedAt != got {
		t.Fatal("creation timestamp mismatch")
	}
}

func TestConversionInputFormats(t *testing.T) {
	rules := []RuleSet{{ID: "r", Rules: []Conversion{{Formats: []string{"flac", "mp3"}, Codec: "opus"}}}}
	for _, format := range []string{"flac", "mp3"} {
		if matched(Track{Format: format}, rules, "r") == nil {
			t.Fatalf("selected format did not match: %s", format)
		}
	}
	if matched(Track{Format: "aac"}, rules, "r") != nil {
		t.Fatal("unselected format matched")
	}
	rules[0].Rules[0] = Conversion{Format: "flac", Codec: "opus"}
	if matched(Track{Format: "mp3"}, rules, "r") != nil {
		t.Fatal("legacy single format ignored")
	}
	if matched(Track{Format: "flac"}, rules, "r") == nil {
		t.Fatal("legacy single format failed")
	}
	rules[0].Rules[0] = Conversion{Formats: []string{}, Codec: "opus"}
	if matched(Track{Format: "aac"}, rules, "r") == nil {
		t.Fatal("empty selection must match any format")
	}
}

func TestConversionKeepsSourceSampleRate(t *testing.T) {
	a := testApp(t)
	input := filepath.Join(t.TempDir(), "source.flac")
	fixture(t, input, "Sample Rate")
	output, release, err := a.cache.Acquire(context.Background(), Track{ID: "keep-rate", Revision: "1", Size: 123}, input, Conversion{Codec: "wav", OutputSampleRate: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	source, err := a.scanner.probe(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := a.scanner.probe(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	if converted.SampleRate != source.SampleRate {
		t.Fatalf("sample rate changed: %d -> %d", source.SampleRate, converted.SampleRate)
	}
}

func TestEmbeddedLyrics(t *testing.T) {
	for _, key := range []string{"lyrics-eng", "LYRICS-XXX", "lyrics-description-eng", "UNSYNCED_LYRICS", "USLT"} {
		if got := embeddedLyrics(map[string]string{key: "[00:01.00]Example"}); got != "[00:01.00]Example" {
			t.Fatalf("%s: %q", key, got)
		}
	}
	tags := map[string]string{"lyrics": "Primary", "lyrics-eng": "Secondary"}
	if got := embeddedLyrics(tags); got != "Primary" {
		t.Fatal(got)
	}
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, ".mp3")+".lrc", []byte("\xef\xbb\xbf \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := localLyrics(path, tags); got != "Primary" {
		t.Fatal(got)
	}
	a := testApp(t)
	if err := a.store.Update(func(st *State) error {
		st.Sources = append(st.Sources, Source{ID: "lyrics-source", Path: filepath.Dir(path)})
		st.Tracks = append(st.Tracks, Track{ID: "lyrics-track", SourceID: "lyrics-source", Path: "song.mp3", Tags: map[string]string{"lyrics-eng": "Embedded"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := request(t, a, "GET", "/api/tracks/lyrics-track/lyrics", nil)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Embedded") {
		t.Fatalf("%d: %s", res.Code, res.Body.String())
	}
}
