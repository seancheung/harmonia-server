package airplay

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestMetadataWire(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	server.SetDeadline(time.Now().Add(time.Second))
	s := &Session{stamp: 100, ctrl: &control{conn: client, rw: client, reader: bufio.NewReader(client), url: "rtsp://receiver/session"}}
	done := make(chan error, 1)
	go func() {
		done <- s.Metadata(TrackMetadata{ID: "track-1", HasArtwork: true, Title: "音乐", Artist: "歌手", Album: "专辑", AlbumArtist: "合辑歌手", Number: 7, Disc: 2, Duration: 180 * time.Second, Position: 30 * time.Second})
	}()
	reader := bufio.NewReader(server)
	progress, err := readMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	start := uint32(100)
	start -= 30 * SampleRate
	want := fmt.Sprintf("progress: %d/100/%d\r\n", start, start+180*SampleRate)
	if string(progress.body) != want || progress.header.Get("RTP-Info") != "rtptime=100" {
		t.Fatalf("incorrect seek progress: %s", progress.body)
	}
	if _, err = fmt.Fprintf(server, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n", progress.header.Get("CSeq")); err != nil {
		t.Fatal(err)
	}
	metadata, err := readMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.header.Get("Content-Type") != "application/x-dmap-tagged" || metadata.header.Get("RTP-Info") != "rtptime=100" {
		t.Fatal("incorrect metadata headers")
	}
	b := metadata.body
	if len(b) < 8 || string(b[:4]) != "mlit" || int(binary.BigEndian.Uint32(b[4:])) != len(b)-8 {
		t.Fatal("invalid container")
	}
	b = b[8:]
	fields := map[string][]byte{}
	var order []string
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatal("truncated field")
		}
		tag := string(b[:4])
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if n > len(b) {
			t.Fatal("invalid field byte length")
		}
		fields[tag] = b[:n]
		order = append(order, tag)
		b = b[n:]
	}
	if order[0] != "mikd" || string(fields["mikd"]) != "\x02" || string(fields["asdk"]) != "\x00" {
		t.Fatal("missing leading music item kind/data kind")
	}
	for tag, want := range map[string]string{"minm": "音乐", "asar": "歌手", "asal": "专辑", "asaa": "合辑歌手", "asac": "\x00\x01", "astn": "\x00\x07", "asdn": "\x00\x02", "astm": "\x00\x02\xbf\x20"} {
		if string(fields[tag]) != want {
			t.Errorf("%s=%x want %x", tag, fields[tag], want)
		}
	}
	if len(fields["mper"]) != 8 || len(fields["miid"]) != 4 {
		t.Fatal("missing track identity")
	}
	if _, err = fmt.Fprintf(server, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n", metadata.header.Get("CSeq")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestArtworkWire(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		data      []byte
	}{
		{"png", "image/png", []byte("\x89PNG\r\n\x1a\nimage")},
		{"jpeg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0}},
		{"clear", "image/none", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			server.SetDeadline(time.Now().Add(time.Second))
			s := &Session{stamp: 12345, ctrl: &control{conn: client, rw: client, reader: bufio.NewReader(client), url: "rtsp://receiver/session"}}
			done := make(chan error, 1)
			go func() { done <- s.Artwork(tc.data) }()
			m, err := readMessage(bufio.NewReader(server))
			if err != nil {
				t.Fatal(err)
			}
			if m.header.Get("Content-Type") != tc.typ || m.header.Get("RTP-Info") != "rtptime=12345" || string(m.body) != string(tc.data) {
				t.Fatal("incorrect artwork request")
			}
			fmt.Fprintf(server, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n", m.header.Get("CSeq"))
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := (&Session{}).Artwork([]byte("not an image")); err == nil {
		t.Fatal("accepted invalid artwork")
	}
	if err := (&Session{}).Artwork(make([]byte, MaxArtworkBytes+1)); err == nil {
		t.Fatal("accepted oversized artwork")
	}
}
