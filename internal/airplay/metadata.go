package airplay

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type TrackMetadata struct {
	ID, Title, Artist, Album, AlbumArtist string
	HasArtwork                            bool
	Number, Disc                          int
	Duration, Position                    time.Duration
}

func dmapItem(tag string, value []byte) []byte {
	b := append([]byte(tag), make([]byte, 4)...)
	binary.BigEndian.PutUint32(b[4:], uint32(len(value)))
	return append(b, value...)
}

func trackDMAP(track TrackMetadata) []byte {
	// DMAP item kind must be the first child of the listing item.
	body := dmapItem("mikd", []byte{2})                 // music
	body = append(body, dmapItem("asdk", []byte{0})...) // local file
	count := uint16(0)
	if track.HasArtwork {
		count = 1
	}
	body = append(body, dmapItem("asac", binary.BigEndian.AppendUint16(nil, count))...)

	for _, field := range []struct{ tag, value string }{{"minm", track.Title}, {"asar", track.Artist}, {"asal", track.Album}, {"asaa", track.AlbumArtist}} {
		body = append(body, dmapItem(field.tag, []byte(field.value))...)
	}
	if track.ID != "" {
		id := sha256.Sum256([]byte(track.ID))
		body = append(body, dmapItem("mper", id[:8])...)
		body = append(body, dmapItem("miid", id[:4])...)
	}
	for _, field := range []struct {
		tag   string
		value int
	}{{"astn", track.Number}, {"asdn", track.Disc}} {
		body = append(body, dmapItem(field.tag, binary.BigEndian.AppendUint16(nil, uint16(min(max(field.value, 0), 65535))))...)
	}
	if track.Duration > 0 {
		body = append(body, dmapItem("astm", binary.BigEndian.AppendUint32(nil, uint32(min(track.Duration.Milliseconds(), int64(1<<32-1)))))...)
	}
	return dmapItem("mlit", body)
}

// Metadata is sent before Stream starts; timestamps use that stream's RTP
// origin, including the saved seek position rather than a new song at zero.
func (s *Session) Metadata(track TrackMetadata) error {
	headers := map[string]string{"RTP-Info": fmt.Sprintf("rtptime=%d", s.stamp)}
	if track.Duration > 0 {
		position := min(max(track.Position, 0), track.Duration)
		start := s.stamp - uint32(position*time.Duration(SampleRate)/time.Second)
		end := start + uint32(track.Duration*time.Duration(SampleRate)/time.Second)
		progress := []byte(fmt.Sprintf("progress: %d/%d/%d\r\n", start, s.stamp, end))
		if _, err := s.ctrl.request("SET_PARAMETER", "", "text/parameters", progress, headers); err != nil {
			return err
		}
	}
	_, err := s.ctrl.request("SET_PARAMETER", "", "application/x-dmap-tagged", trackDMAP(track), headers)
	return err
}

// Artwork sends image bytes over the encrypted control channel. Empty artwork
// explicitly clears the previous cover on receivers supporting image/none.
func (s *Session) Artwork(data []byte) error {
	typ := "image/none"
	if len(data) > 0 {
		if len(data) > MaxArtworkBytes {
			return errors.New("AirPlay artwork exceeds 8 MiB")
		}
		typ = http.DetectContentType(data)
		if typ != "image/jpeg" && typ != "image/png" {
			return errors.New("AirPlay artwork must be JPEG or PNG")
		}
	}
	_, err := s.ctrl.request("SET_PARAMETER", "", typ, data, map[string]string{"RTP-Info": fmt.Sprintf("rtptime=%d", s.stamp)})
	return err
}

const MaxArtworkBytes = 8 << 20
