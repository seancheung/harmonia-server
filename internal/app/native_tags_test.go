package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func commentFixture(values ...string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0))
	binary.Write(&b, binary.LittleEndian, uint32(len(values)))
	for _, v := range values {
		binary.Write(&b, binary.LittleEndian, uint32(len(v)))
		b.WriteString(v)
	}
	return b.Bytes()
}
func flacFixture(comments []byte) []byte {
	n := len(comments)
	return append([]byte{'f', 'L', 'a', 'C', 0x84, byte(n >> 16), byte(n >> 8), byte(n)}, comments...)
}
func safeFixture(n int) []byte {
	return []byte{byte(n>>21) & 127, byte(n>>14) & 127, byte(n>>7) & 127, byte(n) & 127}
}
func id3FrameFixture(id string, payload []byte, flags byte) []byte {
	b := append([]byte(id), safeFixture(len(payload))...)
	b = append(b, 0, flags)
	return append(b, payload...)
}
func id3Fixture(frames []byte, flags byte) []byte {
	b := append([]byte{'I', 'D', '3', 4, 0, flags}, safeFixture(len(frames))...)
	return append(b, frames...)
}
func nativeFixture(t *testing.T, b []byte) map[string][]string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audio")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	tags, err := readNativeTags(p)
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

func TestNativeFLACValues(t *testing.T) {
	tags := nativeFixture(t, flacFixture(commentFixture("ARTIST=AC/DC", "artist=Guest", "GENRE=Rock", "GENRE=Pop", "Album Artist=Various", "Album Artist=Guests")))
	if !reflect.DeepEqual(tags["artist"], []string{"AC/DC", "Guest"}) {
		t.Fatal(tags)
	}
	track := Track{Tags: map[string]string{"artist": "wrong", "genre": "wrong"}, Album: "Compilation"}
	applyNativeTags(&track, tags)
	if track.Artist != "AC/DC; Guest" || track.Genre != "Rock; Pop" || track.AlbumArtist != "Various; Guests" {
		t.Fatal(track)
	}
	if !reflect.DeepEqual(members(track.Artist), []string{"AC/DC", "Guest"}) {
		t.Fatal("slash split without configuration")
	}
	if !reflect.DeepEqual(members(track.Artist, "/"), []string{"AC", "DC", "Guest"}) {
		t.Fatal("extra separator not applied")
	}
	if !reflect.DeepEqual(track.TagValues["genre"], []string{"Rock", "Pop"}) {
		t.Fatal("native boundaries lost")
	}
	other := track
	other.Artist = "Another Artist"
	if albumKey(track) != albumKey(other) {
		t.Fatal("native album artists must control album identity")
	}
	if !(Rule{Field: "genre", Op: "contains", Value: "Pop"}).Match(track) {
		t.Fatal("second genre not searchable")
	}
}

func TestID3v24Values(t *testing.T) {
	frames := id3FrameFixture("TPE1", []byte("\x03AC/DC\x00Guest"), 0)
	// UTF-16LE, with a BOM before each value.
	frames = append(frames, id3FrameFixture("TPE2", []byte{1, 255, 254, 0x68, 0x67, 0, 0, 255, 254, 'B', 0}, 0)...)
	frames = append(frames, id3FrameFixture("TCON", []byte("\x03Rock\x00Pop"), 0)...)
	frames = append(frames, id3FrameFixture("TXXX", []byte("\x03MOOD\x00Calm\x00Happy"), 0)...)
	tags := nativeFixture(t, id3Fixture(frames, 0))
	if !reflect.DeepEqual(tags["artist"], []string{"AC/DC", "Guest"}) {
		t.Fatal(tags)
	}
	if !reflect.DeepEqual(tags["album_artist"], []string{"\u6768", "B"}) {
		t.Fatal(tags)
	}
	if !reflect.DeepEqual(tags["mood"], []string{"Calm", "Happy"}) {
		t.Fatal(tags)
	}
	if !reflect.DeepEqual(tags["genre"], []string{"Rock", "Pop"}) {
		t.Fatal(tags)
	}
}

func TestID3v24ExtendedUnsync(t *testing.T) {
	frame := id3FrameFixture("TPE1", []byte{0, 'A', 255, 0, 225, 0, 'B'}, 2)
	extended := append(safeFixture(6), 1, 0)
	tags := nativeFixture(t, id3Fixture(append(extended, frame...), 0x40))
	if !reflect.DeepEqual(tags["artist"], []string{"A\u00ff\u00e1", "B"}) {
		t.Fatal(tags)
	}
}

func TestID3v24NumericGenres(t *testing.T) {
	tags := nativeFixture(t, id3Fixture(id3FrameFixture("TCON", []byte("\x0317\x0013\x00RX"), 0), 0))
	if !reflect.DeepEqual(tags["genre"], []string{"Rock", "Pop", "Remix"}) {
		t.Fatal(tags)
	}
}

func TestNativeTagScanMigration(t *testing.T) {
	a := testApp(t)
	root := t.TempDir()
	p := filepath.Join(root, "song.flac")
	fixture(t, p, "Migration")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	err = a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "source", Path: root}}
		st.Tracks = []Track{{ID: "song", SourceID: "source", Path: "song.flac", Modified: info.ModTime().UnixNano(), Size: info.Size(), Revision: "old", Favorite: true, PlayCount: 8}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scanNow(t, a, false)
	track := a.store.Read().Tracks[0]
	if track.TagVersion != nativeTagVersion || len(track.TagValues["artist"]) == 0 || track.Revision == "old" {
		t.Fatal("existing metadata was not migrated")
	}
	if !track.Favorite || track.PlayCount != 8 || track.ID != "song" {
		t.Fatal("user data lost during migration")
	}
	scanNow(t, a, false)
	if a.store.Read().Tracks[0].Revision != track.Revision {
		t.Fatal("current metadata was unnecessarily reread")
	}
}

func oggPageFixture(payload []byte, lace []byte, continued bool) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	if continued {
		h[5] = 1
	}
	binary.LittleEndian.PutUint32(h[14:], 1)
	h[26] = byte(len(lace))
	return append(append(h, lace...), payload...)
}
func TestOggRepeatedCommentsAcrossPages(t *testing.T) {
	for _, codec := range []string{"vorbis", "opus"} {
		t.Run(codec, func(t *testing.T) {
			head, prefix := []byte("\x01vorbis"), []byte("\x03vorbis")
			if codec == "opus" {
				head, prefix = []byte("OpusHead"), []byte("OpusTags")
			}
			b := oggPageFixture(head, []byte{byte(len(head))}, false)
			packet := append(prefix, commentFixture("COMMENT="+strings.Repeat("x", 270), "ARTIST=A", "artist=B")...)
			b = append(b, oggPageFixture(packet[:255], []byte{255}, false)...)
			b = append(b, oggPageFixture(packet[255:], []byte{byte(len(packet) - 255)}, true)...)
			tags := nativeFixture(t, b)
			if !reflect.DeepEqual(tags["artist"], []string{"A", "B"}) {
				t.Fatal(tags)
			}
		})
	}
}

func TestMalformedNativeTags(t *testing.T) {
	for _, b := range [][]byte{
		flacFixture([]byte{255, 255, 255, 255}),
		id3Fixture(id3FrameFixture("TPE1", []byte{1, 255}, 0), 0),
		id3Fixture([]byte("TPE1"), 0),
	} {
		p := filepath.Join(t.TempDir(), "audio")
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readNativeTags(p); err == nil {
			t.Fatal("accepted truncated metadata")
		}
	}
}

func TestProbeNativeValues(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	for _, format := range []string{"flac", "mp3"} {
		t.Run(format, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "audio."+format)
			args := []string{"-v", "error", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0", "-metadata", "album=Compilation", "-metadata", "artist=Placeholder", p}
			cmd := exec.Command(ffmpeg, args...)
			cmd.Stdin = bytes.NewReader(make([]byte, 4410*4))
			if out, err := cmd.CombinedOutput(); err != nil {
				if bytes.Contains(out, []byte("Encoder not found")) {
					t.Skip("required fixture encoder unavailable")
				}
				t.Fatalf("ffmpeg: %v %s", err, out)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if format == "flac" {
				// Insert a repeated-comment block; remove the encoder's original comments.
				result := []byte("fLaC")
				pos := 4
				for {
					h := append([]byte{}, b[pos:pos+4]...)
					n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
					last := h[0]&128 != 0
					h[0] &= 127
					if h[0] != 4 {
						result = append(result, h...)
						result = append(result, b[pos+4:pos+4+n]...)
					}
					pos += 4 + n
					if last {
						break
					}
				}
				comments := flacFixture(commentFixture("ARTIST=A", "ARTIST=B", "ALBUM ARTIST=Various", "GENRE=Rock", "GENRE=Pop", "ALBUM=Compilation"))
				result = append(result, comments[4:]...)
				b = append(result, b[pos:]...)
			} else {
				if string(b[:3]) == "ID3" {
					n, _ := syncSafe(b[6:10])
					b = b[10+n:]
				}
				frames := id3FrameFixture("TPE1", []byte("\x03A\x00B"), 0)
				frames = append(frames, id3FrameFixture("TPE2", []byte("\x03Various"), 0)...)
				frames = append(frames, id3FrameFixture("TCON", []byte("\x03Rock\x00Pop"), 0)...)
				b = append(id3Fixture(frames, 0), b...)
			}
			if err = os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			scanner := Scanner{ffprobe: ffprobe}
			track, err := scanner.probe(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if track.Artist != "A; B" || track.Genre != "Rock; Pop" || track.AlbumArtist != "Various" || track.TagVersion != nativeTagVersion {
				t.Fatalf("incorrect parsed tags: %+v", track)
			}
		})
	}
}
