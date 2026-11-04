package airplay

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestPTPSyncOwnToneLayout(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	s := &Session{clock: &clockServer{ptp: true, id: 0x123456789abcdef0}, rtcp: sender, controlTarget: receiver.LocalAddr().(*net.UDPAddr), lead: time.Second}
	for i, stamp := range []uint32{100, 100 + SampleRate} {
		at := time.Unix(1700000000, 0).Add(time.Duration(i) * time.Second)
		if err := s.syncAudio(stamp, at, i == 0); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 100)
		receiver.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := receiver.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		marker := byte(0x80)
		if i == 0 {
			marker = 0x90
		}
		if n != 28 || b[0] != marker || b[1] != 0xd7 || binary.BigEndian.Uint16(b[2:]) != 6 {
			t.Fatalf("header: %x", b[:n])
		}
		if binary.BigEndian.Uint32(b[4:]) != stamp-SampleRate || binary.BigEndian.Uint32(b[16:]) != stamp-11025 {
			t.Fatalf("positions: %x", b[:n])
		}
		if binary.BigEndian.Uint64(b[8:]) != uint64(at.Add(-time.Second).UnixNano()) || binary.BigEndian.Uint64(b[20:]) != s.clock.id {
			t.Fatalf("clock: %x", b[:n])
		}
	}
}

func TestDiscoveryAudioAndMetadataCapabilities(t *testing.T) {
	hosts := map[string]string{"speaker.local.": "192.0.2.1"}
	legacy := &discoveredService{name: "AABBCCDDEEFF@Speaker", host: "speaker.local.", port: 5000, txt: map[string]string{"cn": "1", "md": "0,2"}}
	modern := &discoveredService{name: "Speaker", host: "speaker.local.", port: 7000, txt: map[string]string{"deviceid": "AA:BB:CC:DD:EE:FF", "features": "0x0,0x10000"}}
	services := map[string]*discoveredService{"speaker._airplay._tcp.local.": modern, "speaker._raop._tcp.local.": legacy}
	for range 50 {
		d := devicesFromServices(services, hosts)[0]
		if d.Type != "AirPlay 1" || !d.Metadata.Known || !d.Metadata.Text || !d.Metadata.Progress || d.Metadata.Artwork {
			t.Fatalf("fallback: %+v", d)
		}
	}
	modern.txt["features"] = "0x38200,0x10000"
	d := devicesFromServices(services, hosts)[0]
	if d.Type != "AirPlay 2" || d.Metadata != (MetadataSupport{true, true, true, true}) {
		t.Fatalf("modern: %+v", d)
	}
	a, b := Device{Type: "AirPlay 1"}, Device{Type: "AirPlay 2", UnsupportedReason: "unsupported"}
	if preferDevice(a, b).Type != "AirPlay 1" || preferDevice(b, a).Type != "AirPlay 1" {
		t.Fatal("unsupported transport replaced usable transport")
	}
}

func TestMetadataUnsupportedAndTransportErrors(t *testing.T) {
	for _, code := range []string{"200", "405", "415", "451", "501", "401", "500", "disconnect", "bad-sequence"} {
		t.Run(code, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			server.SetDeadline(time.Now().Add(time.Second))
			s := &Session{ctrl: &control{conn: client, rw: client, reader: bufio.NewReader(client), url: "rtsp://speaker"}}
			done := make(chan error, 1)
			go func() { done <- s.Metadata(TrackMetadata{Title: "test"}) }()
			m, err := readMessage(bufio.NewReader(server))
			if err != nil {
				t.Fatal(err)
			}
			if code == "disconnect" {
				server.Close()
			} else if code == "bad-sequence" {
				fmt.Fprint(server, "RTSP/1.0 451 Unsupported\r\nCSeq: 999\r\n\r\n")
			} else {
				fmt.Fprintf(server, "RTSP/1.0 %s Status\r\nCSeq: %s\r\n\r\n", code, m.header.Get("CSeq"))
			}
			err = <-done
			wantErr := code == "401" || code == "500" || code == "disconnect" || code == "bad-sequence"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v", err)
			}
		})
	}
	// No control object is necessary when the receiver advertises no metadata.
	s := &Session{metadata: MetadataSupport{Known: true}}
	if err := s.Metadata(TrackMetadata{Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := s.Artwork(nil); err != nil {
		t.Fatal(err)
	}
}
