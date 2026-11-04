package airplay

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Known distinguishes advertised capabilities from manually configured devices.
type MetadataSupport struct{ Known, Text, Artwork, Progress bool }

func optionalMetadataError(_ message, err error) error {
	if err == nil {
		return nil
	}
	var rejection *responseError
	if errors.As(err, &rejection) {
		switch rejection.status {
		case "405", "415", "451", "501":
			return nil
		}
	}
	return err
}

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
	return s.metadataAt(track, s.stamp)
}

// MetadataAt schedules a track boundary relative to the continuous stream origin.
// The caller serializes this with Stream reads, including boundaries inside a packet.
func (s *Session) MetadataAt(track TrackMetadata, frame uint64) error {
	return s.metadataAt(track, s.origin+uint32(frame))
}

func (s *Session) metadataAt(track TrackMetadata, stamp uint32) error {
	headers := map[string]string{"RTP-Info": fmt.Sprintf("rtptime=%d", stamp)}
	if track.Duration > 0 && (!s.metadata.Known || s.metadata.Progress) {
		position := min(max(track.Position, 0), track.Duration)
		start := stamp - uint32(position*time.Duration(SampleRate)/time.Second)
		end := start + uint32(track.Duration*time.Duration(SampleRate)/time.Second)
		progress := []byte(fmt.Sprintf("progress: %d/%d/%d\r\n", start, stamp, end))
		m, err := s.ctrl.request("SET_PARAMETER", "", "text/parameters", progress, headers)
		if err = optionalMetadataError(m, err); err != nil {
			return err
		}
	}
	if s.metadata.Known && !s.metadata.Text {
		return nil
	}
	track.HasArtwork = track.HasArtwork && (!s.metadata.Known || s.metadata.Artwork)
	m, err := s.ctrl.request("SET_PARAMETER", "", "application/x-dmap-tagged", trackDMAP(track), headers)
	return optionalMetadataError(m, err)
}

var ErrArtworkTransport = errors.New("AirPlay artwork transport failed")

// Artwork sends image bytes over the encrypted control channel. Empty artwork
// explicitly clears the previous cover on receivers supporting image/none.
func (s *Session) Artwork(data []byte) error {
	return s.artworkAt(data, s.stamp)
}

func (s *Session) ArtworkAt(data []byte, frame uint64) error {
	return s.artworkAt(data, s.origin+uint32(frame))
}

func (s *Session) artworkAt(data []byte, stamp uint32) error {
	if s.metadata.Known && !s.metadata.Artwork {
		return nil
	}
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
	response, err := s.ctrl.request("SET_PARAMETER", "", typ, data, map[string]string{"RTP-Info": fmt.Sprintf("rtptime=%d", stamp)})
	if err != nil && response.line == "" {
		// A missing response leaves the control stream unusable, unlike a
		// complete RTSP rejection of optional artwork.
		s.fail(err)
		return fmt.Errorf("%w: %w", ErrArtworkTransport, err)
	}
	return err
}

const MaxArtworkBytes = 8 << 20
