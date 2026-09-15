package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type Source struct {
	FolderTimes map[string]FileTimes `json:"folderTimes,omitempty"`
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Path        string               `json:"path"`
	Keep        []string             `json:"keep"`
	Ignore      []string             `json:"ignore"`
	Error       string               `json:"error"`
}
type FileTimes struct {
	ModifiedAt int64 `json:"modifiedAt"`
	CreatedAt  int64 `json:"createdAt"`
}
type Track struct {
	ModifiedAt      int64             `json:"modifiedAt"`
	CreatedAt       int64             `json:"createdAt"`
	ID              string            `json:"id"`
	SourceID        string            `json:"sourceId"`
	Path            string            `json:"path"`
	Filename        string            `json:"filename"`
	Folder          string            `json:"folder"`
	Title           string            `json:"title"`
	Artist          string            `json:"artist"`
	Album           string            `json:"album"`
	AlbumArtist     string            `json:"albumArtist"`
	AlbumID         string            `json:"albumId"`
	Genre           string            `json:"genre"`
	Year            int               `json:"year"`
	Disc            int               `json:"disc"`
	Number          int               `json:"number"`
	Duration        float64           `json:"duration"`
	Bitrate         int               `json:"bitrate"`
	SampleRate      int               `json:"sampleRate"`
	Format          string            `json:"format"`
	AddedAt         int64             `json:"addedAt"`
	Modified        int64             `json:"modified"`
	Size            int64             `json:"size"`
	Revision        string            `json:"revision"`
	Favorite        bool              `json:"favorite"`
	PlayCount       int               `json:"playCount"`
	LastPlayed      int64             `json:"lastPlayed"`
	Missing         bool              `json:"missing"`
	Cover           string            `json:"cover"`
	HasCover        bool              `json:"hasCover"`
	ArtworkRevision string            `json:"artworkRevision"`
	Lyrics          string            `json:"lyrics"`
	Tags            map[string]string `json:"tags"`
	TrackGain       *float64          `json:"trackGain"`
	AlbumGain       *float64          `json:"albumGain"`
	TrackPeak       *float64          `json:"trackPeak"`
	AlbumPeak       *float64          `json:"albumPeak"`
}
type Rule struct {
	Mode      string `json:"mode,omitempty"`
	Rules     []Rule `json:"rules,omitempty"`
	Field     string `json:"field,omitempty"`
	Op        string `json:"op,omitempty"`
	Value     any    `json:"value,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}
type Playlist struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Smart  bool     `json:"smart"`
	Rule   *Rule    `json:"rule,omitempty"`
	Sort   string   `json:"sort"`
	Desc   bool     `json:"desc"`
	Tracks []string `json:"tracks"`
}
type Conversion struct {
	Formats          []string `json:"formats,omitempty"`
	GainIdentity     string   `json:"-"`
	Format           string   `json:"format"`
	BitrateOp        string   `json:"bitrateOp"`
	Bitrate          int      `json:"bitrate"`
	SampleRateOp     string   `json:"sampleRateOp"`
	SampleRate       int      `json:"sampleRate"`
	Codec            string   `json:"codec"`
	OutputBitrate    int      `json:"outputBitrate"`
	OutputSampleRate int      `json:"outputSampleRate"`
}
type RuleSet struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Rules []Conversion `json:"rules"`
}
type State struct {
	Sources    []Source        `json:"sources"`
	Tracks     []Track         `json:"tracks"`
	Playlists  []Playlist      `json:"playlists"`
	RuleSets   []RuleSet       `json:"ruleSets"`
	CacheLimit int64           `json:"cacheLimit"`
	Sessions   map[string]bool `json:"sessions"`
}

func newID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func members(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range strings.Split(s, ";") {
		v = strings.TrimSpace(v)
		if v != "" && !seen[strings.ToLower(v)] {
			out = append(out, v)
			seen[strings.ToLower(v)] = true
		}
	}
	return out
}
func albumArtistTag(tags map[string]string) string {
	for _, key := range []string{"album_artist", "albumartist", "album artist", "album-artist"} {
		if value := strings.TrimSpace(tags[key]); value != "" {
			return value
		}
	}
	return ""
}

func albumKey(t Track) string {
	a := t.AlbumArtist
	if strings.TrimSpace(a) == "" {
		a = t.Artist
	}
	m := members(strings.ToLower(a))
	if len(m) == 0 {
		m = []string{"unknown artist"}
	}
	sort.Strings(m)
	name := strings.ToLower(strings.TrimSpace(t.Album))
	if name == "" {
		name = "unknown album"
	}
	b, _ := json.Marshal([]any{name, m, t.Year})
	return hex.EncodeToString([]byte(string(b)))
}
func value(t Track, f string) any {
	switch f {
	case "title":
		return t.Title
	case "artist":
		return t.Artist
	case "album":
		return t.Album
	case "genre":
		return t.Genre
	case "albumArtist":
		return t.AlbumArtist
	case "year":
		return float64(t.Year)
	case "duration":
		return t.Duration
	case "playCount":
		return float64(t.PlayCount)
	case "addedAt":
		return float64(t.AddedAt)
	case "modifiedAt":
		return float64(t.ModifiedAt)
	case "createdAt":
		return float64(t.CreatedAt)
	case "lastPlayed":
		return float64(t.LastPlayed)
	case "disc":
		return float64(t.Disc)
	case "number":
		return float64(t.Number)
	case "bitrate":
		return float64(t.Bitrate)
	case "sampleRate":
		return float64(t.SampleRate)
	case "filename":
		return t.Filename
	case "favorite":
		return t.Favorite
	case "format":
		return t.Format
	case "bpm":
		for _, name := range []string{"bpm", "tbpm", "tempo"} {
			if n, ok := numeric(strings.TrimSpace(t.Tags[name])); ok && n > 0 {
				return n
			}
		}
		return nil
	case "key":
		for _, name := range []string{"initialkey", "initial_key", "tkey", "key"} {
			if key := strings.TrimSpace(t.Tags[name]); key != "" {
				return key
			}
		}
		return ""
	default:
		return t.Tags[strings.TrimPrefix(f, "tag:")]
	}
}
func numeric(v any) (float64, bool) {
	f, e := strconv.ParseFloat(fmt.Sprint(v), 64)
	return f, e == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}
func compare(a, b any, op string) bool {
	if a == nil {
		return op == "ne"
	}
	if op == "contains" || op == "notContains" {
		contains := strings.Contains(strings.ToLower(fmt.Sprint(a)), strings.ToLower(fmt.Sprint(b)))
		if op == "notContains" {
			return !contains
		}
		return contains
	}
	c := strings.Compare(strings.ToLower(fmt.Sprint(a)), strings.ToLower(fmt.Sprint(b)))
	if av, ok := a.(float64); ok {
		bv, valid := numeric(b)
		if !valid {
			return false
		}
		c = 0
		if av < bv {
			c = -1
		}
		if av > bv {
			c = 1
		}
	}
	switch op {
	case "eq":
		return c == 0
	case "ne":
		return c != 0
	case "gt":
		return c > 0
	case "gte":
		return c >= 0
	case "lt":
		return c < 0
	case "lte":
		return c <= 0
	}
	return false
}
func (r Rule) Match(t Track) bool {
	if r.Mode != "" {
		for _, c := range r.Rules {
			m := c.Match(t)
			if r.Mode == "all" && !m {
				return false
			}
			if r.Mode == "any" && m {
				return true
			}
		}
		return r.Mode == "all"
	}
	if r.Field == "folder" {
		folder := strings.Trim(fmt.Sprint(r.Value), "/")
		match := t.SourceID == r.SourceID && (t.Folder == folder || (r.Recursive && (folder == "" || strings.HasPrefix(t.Folder, folder+"/"))))
		if r.Op == "ne" {
			return !match
		}
		return match
	}
	return compare(value(t, r.Field), r.Value, r.Op)
}
func (r Rule) Validate() error {
	count := 0
	var walk func(Rule, int) error
	walk = func(r Rule, depth int) error {
		count++
		if count > 30 || depth > 4 {
			return fmt.Errorf("rules exceed 30 nodes or 4 group levels")
		}
		if r.Mode != "" {
			if (r.Mode != "all" && r.Mode != "any") || len(r.Rules) == 0 {
				return fmt.Errorf("invalid or empty group")
			}
			for _, c := range r.Rules {
				d := depth
				if c.Mode != "" {
					d++
				}
				if e := walk(c, d); e != nil {
					return e
				}
			}
			return nil
		}
		if r.Field == "folder" {
			if r.SourceID == "" || (r.Op != "eq" && r.Op != "ne") || strings.Contains(fmt.Sprint(r.Value), "..") {
				return fmt.Errorf("invalid folder rule")
			}
			return nil
		}
		if !strings.Contains("|title|artist|album|albumArtist|genre|year|duration|playCount|addedAt|disc|number|bitrate|sampleRate|bpm|key|favorite|format|", "|"+r.Field+"|") && !strings.HasPrefix(r.Field, "tag:") {
			return fmt.Errorf("unknown rule field")
		}
		if !strings.Contains("|contains|notContains|eq|ne|gt|gte|lt|lte|", "|"+r.Op+"|") {
			return fmt.Errorf("invalid operator")
		}
		if strings.Contains("|year|duration|playCount|addedAt|disc|number|bitrate|sampleRate|bpm|", "|"+r.Field+"|") {
			if _, ok := numeric(r.Value); !ok {
				return fmt.Errorf("invalid number")
			}
		}
		return nil
	}
	return walk(r, 1)
}
func missing(v any, f string) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	if n, ok := v.(float64); ok {
		return n == 0 && f != "playCount"
	}
	return false
}
func sortTracks(ts []Track, field string, desc bool) {
	if field == "" {
		field = "addedAt"
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		fields := []string{field, "artist", "album", "disc", "number", "title"}
		seen := map[string]bool{}
		for _, f := range fields {
			if seen[f] {
				continue
			}
			seen[f] = true
			av, bv := value(a, f), value(b, f)
			am, bm := missing(av, f), missing(bv, f)
			if am != bm {
				return !am
			}
			if am && bm {
				continue
			}
			if reflect.DeepEqual(av, bv) {
				continue
			}
			less := compare(av, bv, "lt")
			if f == field && desc {
				less = compare(av, bv, "gt")
			}
			if compare(av, bv, "eq") {
				continue
			}
			return less
		}
		return a.ID < b.ID
	})
}
func folderOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}
