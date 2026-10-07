package airplay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func TestNTPSyncTracksTransmissionHead(t *testing.T) {
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
	s := &Session{clock: &clockServer{}, rtcp: sender, controlTarget: receiver.LocalAddr().(*net.UDPAddr), lead: time.Second}
	start := time.Unix(1700000000, 0)
	// Include wraparound: subtraction is modulo 2^32, never clamped to zero.
	for i, stamp := range []uint32{100, 100 + SampleRate} {
		audible := start.Add(time.Duration(i) * time.Second)
		if err := s.syncAudio(stamp, audible, i == 0); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 100)
		receiver.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := receiver.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		if n != 20 || b[1] != 0xd4 || binary.BigEndian.Uint16(b[2:]) != 7 {
			t.Fatalf("bad sync header: %x", b[:n])
		}
		if got := binary.BigEndian.Uint32(b[16:]); got != stamp {
			t.Fatalf("transmission head=%d, want %d", got, stamp)
		}
		if got := binary.BigEndian.Uint32(b[4:]); got != stamp-uint32(SampleRate) {
			t.Fatalf("incorrect audible position: %d", got)
		}
		if got := binary.BigEndian.Uint64(b[8:]); got != ntp(audible.Add(-time.Second)) {
			t.Fatal("clock instant does not match audible position")
		}
	}
}

func TestNTPDoesNotAdvanceWithoutTiming(t *testing.T) {
	s := &Session{clock: &clockServer{}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	advanced := false
	err := s.Stream(ctx, bytes.NewReader(make([]byte, PCMBytes)), func(time.Duration) { advanced = true })
	if !errors.Is(err, context.DeadlineExceeded) || advanced {
		t.Fatalf("error=%v advanced=%v", err, advanced)
	}
	s.clock.lastProbe.Store(time.Now().UnixNano())
	if err := s.waitForClock(context.Background()); err != nil {
		t.Fatal(err)
	}
}
