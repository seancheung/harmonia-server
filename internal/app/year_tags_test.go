package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProbeYearDateFormats(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	for _, format := range []string{"flac", "mp3"} {
		for _, key := range []string{"date", "year", "TYER"} {
			for _, date := range []string{"2013", "2013-01", "2013-01-12"} {
				t.Run(format+"/"+key+"/"+date, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "song."+format)
					cmd := exec.Command(ffmpeg, "-v", "error", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0", "-metadata", key+"="+date, path)
					cmd.Stdin = bytes.NewReader(make([]byte, 4410*4))
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("fixture: %v %s", err, out)
					}
					scanner := Scanner{ffprobe: ffprobe}
					track, err := scanner.probe(context.Background(), path)
					if err != nil {
						t.Fatal(err)
					}
					if track.Year != 2013 {
						t.Fatalf("year=%d tags=%v", track.Year, track.Tags)
					}
				})
			}
		}
	}
}

func TestYearTagAliasesAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags map[string][]string
		want int
	}{
		{"legacy complete date", map[string][]string{"tyer": {"2002-06-25"}}, 2002},
		{"prefer date", map[string][]string{"date": {"2013-01"}, "year": {"2002"}, "tyer": {"1999"}}, 2013},
		{"invalid date fallback", map[string][]string{"date": {"unknown"}, "year": {"2013-01-12"}}, 2013},
		{"multi value fallback", map[string][]string{"tyer": {"", "unknown", " 2002-06-25 "}}, 2002},
		{"unknown", map[string][]string{"date": {"0000", "unknown", "12", "+201"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tagYear(tc.tags); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	tags := nativeFixture(t, id3Fixture(id3FrameFixture("TYER", []byte("\x032002-06-25"), 0), 0))
	if tagYear(tags) != 2002 || tags["tyer"][0] != "2002-06-25" {
		t.Fatalf("native TYER lost: %v", tags)
	}
}

func TestYearFixRefreshesUnchangedScannedTrack(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err = exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	root := t.TempDir()
	p := filepath.Join(root, "song.mp3")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0", "-metadata", "TYER=2002-06-25", "-metadata", "album=By The Way", p)
	cmd.Stdin = bytes.NewReader(make([]byte, 4410*4))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	if err = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Path: root}}
		st.Tracks = []Track{{ID: "song", SourceID: "source", Path: "song.mp3", Album: "By The Way", Year: 0, Tags: map[string][]string{"tyer": {"2002-06-25"}}, TagVersion: 2, Modified: info.ModTime().UnixNano(), Size: info.Size(), Favorite: true, PlayCount: 8, Revision: "old"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	scanNow(t, a, false)
	track := a.store.Read().Tracks[0]
	if track.Year != 2002 || track.ID != "song" || !track.Favorite || track.PlayCount != 8 || track.Revision == "old" {
		t.Fatalf("incorrect refresh: %+v", track)
	}
	var albumYear int
	if err = a.store.db.QueryRow("SELECT year FROM albums WHERE id=?", track.AlbumID).Scan(&albumYear); err != nil || albumYear != 2002 {
		t.Fatalf("album year %d: %v", albumYear, err)
	}
	scanNow(t, a, false)
	if a.store.Read().Tracks[0].Revision != track.Revision {
		t.Fatal("metadata reread more than once")
	}
}
