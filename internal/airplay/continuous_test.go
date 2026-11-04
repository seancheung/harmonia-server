package airplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestContinuousAudioPacketsAcrossTrackAndSequenceBoundaries(t *testing.T) {
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	sender, receiver, controlSender, controlReceiver := listen(), listen(), listen(), listen()
	key := bytes.Repeat([]byte{42}, 32)
	s := &Session{secret: key, seq: 65535, stamp: 0xffffff00, ssrc: 123, lead: 10 * time.Millisecond, clock: &clockServer{}, data: sender, rtcp: controlSender, target: receiver.LocalAddr().(*net.UDPAddr), controlTarget: controlReceiver.LocalAddr().(*net.UDPAddr), packets: map[uint16][]byte{}}
	s.clock.lastProbe.Store(time.Now().UnixNano())
	first := bytes.Repeat([]byte{1, 2, 3, 4}, Frames-3)
	second := bytes.Repeat([]byte{5, 6, 7, 8}, Frames+9)
	pcm := append(append([]byte{}, first...), second...)
	done := make(chan error, 1)
	var progress time.Duration
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		done <- s.Stream(ctx, io.MultiReader(bytes.NewReader(first), bytes.NewReader(second)), func(p time.Duration) { progress = p })
	}()
	for i := 0; i < 3; i++ {
		receiver.SetReadDeadline(time.Now().Add(time.Second))
		packet := make([]byte, 2048)
		n, _, err := receiver.ReadFromUDP(packet)
		if err != nil {
			t.Fatal(err)
		}
		packet = packet[:n]
		if binary.BigEndian.Uint16(packet[2:]) != uint16(65535+i) || binary.BigEndian.Uint32(packet[4:]) != uint32(uint64(0xffffff00)+uint64(i*Frames)) {
			t.Fatalf("RTP continuity: %x", packet[:12])
		}
		if (packet[1]&0x80 != 0) != (i == 0) {
			t.Fatal("track boundary reset start marker")
		}
		nonce := make([]byte, 12)
		copy(nonce[4:], packet[n-8:])
		if binary.LittleEndian.Uint64(nonce[4:]) != uint64(i) {
			t.Fatal("nonce reset")
		}
		raw, err := aead(key).Open(nil, nonce, packet[12:n-8], packet[4:12])
		if err != nil {
			t.Fatal(err)
		}
		want := make([]byte, PCMBytes)
		copy(want, pcm[i*PCMBytes:])
		if !bytes.Equal(raw, alac(want)) {
			t.Fatalf("packet %d inserted silence or lost PCM at song boundary", i)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if progress != time.Duration(len(pcm)/4)*time.Second/SampleRate {
		t.Fatalf("progress includes padding: %v", progress)
	}
}

func TestQueuedMetadataUsesExactSampleOrigin(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	server.SetDeadline(time.Now().Add(time.Second))
	s := &Session{origin: 0xfffffff0, stamp: 999, metadata: MetadataSupport{Known: true, Text: true}, ctrl: &control{conn: client, rw: client, reader: bufio.NewReader(client), url: "rtsp://speaker"}}
	done := make(chan error, 1)
	go func() { done <- s.MetadataAt(TrackMetadata{Title: "second"}, 400) }()
	m, err := readMessage(bufio.NewReader(server))
	if err != nil {
		t.Fatal(err)
	}
	if m.header.Get("RTP-Info") != "rtptime=384" {
		t.Fatalf("boundary rounded to packet: %s", m.header.Get("RTP-Info"))
	}
	fmt.Fprintf(server, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n", m.header.Get("CSeq"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
