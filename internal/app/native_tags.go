package app

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const nativeTagVersion = 2
const maxNativeTags = 64 * 1024 * 1024

// Keep field boundaries before ffprobe flattens repeated comments or text values.
func readNativeTags(path string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tags := map[string][]string{}
	header := make([]byte, 10)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if n >= 10 && string(header[:3]) == "ID3" {
		size, err := syncSafe(header[6:10])
		if err != nil {
			return nil, err
		}
		if size > maxNativeTags {
			return nil, fmt.Errorf("ID3 metadata exceeds size limit")
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(f, data); err != nil {
			return nil, err
		}
		if header[3] == 4 {
			if err = readID3v24(data, header[5], tags); err != nil {
				return nil, err
			}
		}
		if header[3] == 4 && header[5]&0x10 != 0 {
			if _, err = f.Seek(10, io.SeekCurrent); err != nil {
				return nil, err
			}
		}
	} else {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	magic := make([]byte, 4)
	if _, err = io.ReadFull(f, magic); err != nil {
		return tags, nil
	}
	switch string(magic) {
	case "fLaC":
		for consumed := 0; consumed < maxNativeTags; {
			h := make([]byte, 4)
			if _, err = io.ReadFull(f, h); err != nil {
				return nil, err
			}
			size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
			consumed += 4 + size
			if consumed > maxNativeTags {
				return nil, fmt.Errorf("FLAC metadata exceeds size limit")
			}
			if h[0]&127 == 4 {
				b := make([]byte, size)
				if _, err = io.ReadFull(f, b); err != nil {
					return nil, err
				}
				if err = readComments(b, tags); err != nil {
					return nil, err
				}
			} else if _, err = f.Seek(int64(size), io.SeekCurrent); err != nil {
				return nil, err
			}
			if h[0]&128 != 0 {
				return tags, nil
			}
		}
		return nil, fmt.Errorf("FLAC metadata exceeds size limit")
	case "OggS":
		if _, err = f.Seek(-4, io.SeekCurrent); err != nil {
			return nil, err
		}
		return readOggComments(f, tags)
	}
	return tags, nil
}

func addNativeTag(tags map[string][]string, key, value string) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "metadata_block_picture" || key == "coverart" {
		return
	}
	value = strings.TrimSpace(value)
	if key != "" && value != "" {
		tags[key] = append(tags[key], value)
	}
}

func readComments(b []byte, tags map[string][]string) error {
	take := func() ([]byte, error) {
		if len(b) < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		n := uint64(binary.LittleEndian.Uint32(b))
		b = b[4:]
		if n > uint64(len(b)) {
			return nil, io.ErrUnexpectedEOF
		}
		v := b[:int(n)]
		b = b[int(n):]
		return v, nil
	}
	if _, err := take(); err != nil {
		return err
	}
	if len(b) < 4 {
		return io.ErrUnexpectedEOF
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	for i := uint32(0); i < count; i++ {
		v, err := take()
		if err != nil {
			return err
		}
		key, value, ok := strings.Cut(string(v), "=")
		if !ok || !utf8.Valid(v) {
			return fmt.Errorf("invalid Vorbis comment")
		}
		addNativeTag(tags, key, value)
	}
	return nil
}

func readOggComments(r io.Reader, tags map[string][]string) (map[string][]string, error) {
	packets := map[uint32][]byte{}
	codec := map[uint32]string{}
	for consumed := 0; consumed < maxNativeTags; {
		h := make([]byte, 27)
		_, err := io.ReadFull(r, h)
		if err == io.EOF {
			return tags, nil
		}
		if err != nil {
			return nil, err
		}
		if string(h[:4]) != "OggS" || h[4] != 0 {
			return nil, fmt.Errorf("invalid Ogg page")
		}
		serial := binary.LittleEndian.Uint32(h[14:18])
		lace := make([]byte, int(h[26]))
		if _, err = io.ReadFull(r, lace); err != nil {
			return nil, err
		}
		consumed += 27 + len(lace)
		for _, n := range lace {
			consumed += int(n)
			if consumed > maxNativeTags {
				return nil, fmt.Errorf("Ogg headers exceed size limit")
			}
			b := make([]byte, int(n))
			if _, err = io.ReadFull(r, b); err != nil {
				return nil, err
			}
			packets[serial] = append(packets[serial], b...)
			if n == 255 {
				continue
			}
			packet := packets[serial]
			delete(packets, serial)
			if bytes.HasPrefix(packet, []byte("\x01vorbis")) {
				codec[serial] = "vorbis"
				continue
			}
			if bytes.HasPrefix(packet, []byte("OpusHead")) {
				codec[serial] = "opus"
				continue
			}
			offset := 0
			if codec[serial] == "vorbis" && bytes.HasPrefix(packet, []byte("\x03vorbis")) {
				offset = 7
			}
			if codec[serial] == "opus" && bytes.HasPrefix(packet, []byte("OpusTags")) {
				offset = 8
			}
			if offset > 0 {
				if err = readComments(packet[offset:], tags); err != nil {
					return nil, err
				}
				return tags, nil
			}
		}
	}
	return nil, fmt.Errorf("Ogg headers exceed size limit")
}

func syncSafe(b []byte) (int, error) {
	if len(b) != 4 {
		return 0, io.ErrUnexpectedEOF
	}
	n := 0
	for _, v := range b {
		if v&128 != 0 {
			return 0, fmt.Errorf("invalid synchsafe integer")
		}
		n = n<<7 | int(v)
	}
	return n, nil
}
func undoUnsync(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 255 && i+1 < len(b) && b[i+1] == 0 {
			i++
		}
	}
	return out
}
func id3Text(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	encoding := b[0]
	b = b[1:]
	var text string
	switch encoding {
	case 0:
		runes := make([]rune, len(b))
		for i, v := range b {
			runes[i] = rune(v)
		}
		text = string(runes)
	case 3:
		if !utf8.Valid(b) {
			return nil, fmt.Errorf("invalid ID3 UTF-8")
		}
		text = string(b)
	case 1, 2:
		var order binary.ByteOrder = binary.BigEndian
		if encoding == 1 {
			if len(b) < 2 {
				return nil, io.ErrUnexpectedEOF
			}
			if b[0] == 255 && b[1] == 254 {
				order = binary.LittleEndian
			} else if b[0] != 254 || b[1] != 255 {
				return nil, fmt.Errorf("missing ID3 UTF-16 BOM")
			}
			b = b[2:]
		}
		if len(b)%2 != 0 {
			return nil, fmt.Errorf("invalid ID3 UTF-16 length")
		}
		values := make([]uint16, 0, len(b)/2)
		for i := 0; i < len(b); i += 2 {
			v := order.Uint16(b[i:])
			if v == 0xfeff {
				continue
			}
			values = append(values, v)
		}
		text = string(utf16.Decode(values))
	default:
		return nil, fmt.Errorf("unsupported ID3 text encoding")
	}
	return strings.Split(text, "\x00"), nil
}

func readID3v24(b []byte, flags byte, tags map[string][]string) error {
	if flags&0x40 != 0 {
		if len(b) < 4 {
			return io.ErrUnexpectedEOF
		}
		n, err := syncSafe(b[:4])
		if err != nil {
			return err
		}
		if n < 6 || n > len(b) {
			return fmt.Errorf("invalid ID3 extended header")
		}
		b = b[n:]
	}
	names := map[string]string{"TPE1": "artist", "TPE2": "album_artist", "TCON": "genre", "TALB": "album", "TIT2": "title", "TCOM": "composer", "TPE3": "conductor", "TDRC": "date", "TRCK": "track", "TPOS": "disc", "TBPM": "bpm", "TKEY": "initialkey"}
	for len(b) > 0 {
		if b[0] == 0 {
			return nil
		}
		if len(b) < 10 {
			return io.ErrUnexpectedEOF
		}
		id := string(b[:4])
		n, err := syncSafe(b[4:8])
		if err != nil {
			return err
		}
		format := b[9]
		b = b[10:]
		if n > len(b) {
			return io.ErrUnexpectedEOF
		}
		frame := b[:n]
		b = b[n:]
		key, known := names[id]
		if !known && id != "TXXX" {
			continue
		}
		if format&4 != 0 {
			return fmt.Errorf("encrypted ID3 text frame")
		}
		if flags&128 != 0 || format&2 != 0 {
			frame = undoUnsync(frame)
		}
		if format&0x40 != 0 {
			if len(frame) < 1 {
				return io.ErrUnexpectedEOF
			}
			frame = frame[1:]
		}
		if format&1 != 0 {
			if len(frame) < 4 {
				return io.ErrUnexpectedEOF
			}
			frame = frame[4:]
		}
		if format&8 != 0 {
			zr, err := zlib.NewReader(bytes.NewReader(frame))
			if err != nil {
				return err
			}
			frame, err = io.ReadAll(io.LimitReader(zr, maxNativeTags+1))
			zr.Close()
			if err != nil {
				return err
			}
			if len(frame) > maxNativeTags {
				return fmt.Errorf("ID3 frame exceeds size limit")
			}
		}
		values, err := id3Text(frame)
		if err != nil {
			return err
		}
		if id == "TXXX" {
			if len(values) < 2 {
				continue
			}
			key = values[0]
			values = values[1:]
		}
		for _, value := range values {
			if id == "TCON" {
				value = id3Genre(value)
			}
			addNativeTag(tags, key, value)
		}
	}
	return nil
}

// ID3v2.4 retains the numeric ID3v1 genre vocabulary as well as free text.
func id3Genre(value string) string {
	genres := strings.Split("Blues|Classic Rock|Country|Dance|Disco|Funk|Grunge|Hip-Hop|Jazz|Metal|New Age|Oldies|Other|Pop|R&B|Rap|Reggae|Rock|Techno|Industrial|Alternative|Ska|Death Metal|Pranks|Soundtrack|Euro-Techno|Ambient|Trip-Hop|Vocal|Jazz+Funk|Fusion|Trance|Classical|Instrumental|Acid|House|Game|Sound Clip|Gospel|Noise|AlternRock|Bass|Soul|Punk|Space|Meditative|Instrumental Pop|Instrumental Rock|Ethnic|Gothic|Darkwave|Techno-Industrial|Electronic|Pop-Folk|Eurodance|Dream|Southern Rock|Comedy|Cult|Gangsta|Top 40|Christian Rap|Pop/Funk|Jungle|Native American|Cabaret|New Wave|Psychadelic|Rave|Showtunes|Trailer|Lo-Fi|Tribal|Acid Punk|Acid Jazz|Polka|Retro|Musical|Rock & Roll|Hard Rock", "|")
	if value == "RX" {
		return "Remix"
	}
	if value == "CR" {
		return "Cover"
	}
	if n, err := strconv.Atoi(value); err == nil && n >= 0 && n < len(genres) {
		return genres[n]
	}
	return value
}

func applyNativeTags(t *Track, native map[string][]string) {
	if t.Tags == nil {
		t.Tags = map[string][]string{}
	}
	for key, values := range native {
		t.Tags[key] = append([]string{}, values...)
	}
	t.Artist = tagText(t.Tags, "artist")
	t.AlbumArtist = albumArtistTag(tagStrings(t.Tags))
	t.Genre = tagText(t.Tags, "genre")
	t.TagVersion = nativeTagVersion
}
