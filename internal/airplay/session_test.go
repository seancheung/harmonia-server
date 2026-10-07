package airplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// Exercise real TCP/UDP sockets, a separately calculated SRP receiver, encrypted
// RTSP, reverse-channel acknowledgement, and authenticated audio/retransmission.
func TestTransientSessionWire(t *testing.T) {
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	eventListener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer eventListener.Close()
	data, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	defer data.Close()
	controlUDP, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	defer controlUDP.Close()
	serverDone := make(chan error, 1)
	eventDone := make(chan error, 1)
	audioKey := make(chan []byte, 1)
	senderPort := make(chan int, 1)
	go func() {
		serverDone <- func() error {
			conn, e := listener.Accept()
			if e != nil {
				return e
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			var rw io.ReadWriter = conn
			reader := bufio.NewReader(rw)
			reply := func(m message, body []byte) error {
				return writeAll(rw, append([]byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %s\r\nContent-Length: %d\r\n\r\n", m.header.Get("CSeq"), len(body))), body...))
			}
			m, e := readMessage(reader)
			if e != nil {
				return e
			}
			if !strings.HasPrefix(m.line, "GET /info ") {
				return errors.New("expected info")
			}
			if e = reply(m, plist(map[string]any{"features": uint64(1 << 48)})); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			fields, e := untlv(m.body)
			if e != nil {
				return e
			}
			if m.header.Get("X-Apple-HKP") != "4" || !bytes.Equal(fields[19], []byte{16, 0, 0, 0}) {
				return errors.New("expected transient HAP pairing")
			}
			n, _ := new(big.Int).SetString(srpPrime, 16)
			g := big.NewInt(5)
			salt := []byte("receiver-salt-123")
			b := big.NewInt(123456789)
			x := integer(hash(salt, hash([]byte("Pair-Setup:3939"))))
			v := new(big.Int).Exp(g, x, n)
			k := integer(hash(pad(n), pad(g)))
			B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(g, b, n))
			B.Mod(B, n)
			if e = reply(m, tlv(tag(6), tag(2), tag(2), salt, tag(3), pad(B))); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			fields, e = untlv(m.body)
			if e != nil {
				return e
			}
			A := integer(fields[3])
			u := integer(hash(pad(A), pad(B)))
			S := new(big.Int).Mul(A, new(big.Int).Exp(v, u, n))
			S.Exp(S, b, n)
			secret := hash(S.Bytes())
			hn, hg := hash(n.Bytes()), hash(g.Bytes())
			for i := range hn {
				hn[i] ^= hg[i]
			}
			proof := hash(hn, hash([]byte("Pair-Setup")), salt, A.Bytes(), B.Bytes(), secret)
			if !bytes.Equal(proof, fields[4]) {
				return errors.New("incorrect controller SRP proof")
			}
			if e = reply(m, tlv(tag(6), tag(4), tag(4), hash(A.Bytes(), proof, secret))); e != nil {
				return e
			}
			secure := newSecure(structRW{reader, conn}, secret, false)
			secure.read, secure.write = secure.write, secure.read
			rw = secure
			reader = bufio.NewReader(rw)
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			value, e := unplist(m.body)
			if e != nil {
				return e
			}
			session := value.(map[string]any)
			if session["timingProtocol"] != "NTP" {
				return errors.New("expected NTP session")
			}
			timing, e := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(number(session["timingPort"]))})
			if e != nil {
				return e
			}
			defer timing.Close()
			_ = timing.SetDeadline(time.Now().Add(time.Second))
			probe := make([]byte, 32)
			probe[0] = 0x80
			probe[1] = 0xd2
			binary.BigEndian.PutUint64(probe[24:], 123)
			_, _ = timing.Write(probe)
			buf := make([]byte, 32)
			if _, e = timing.Read(buf); e != nil {
				return e
			}
			if binary.BigEndian.Uint64(buf[8:]) != 123 || buf[1] != 0xd3 {
				return errors.New("wrong timing reply")
			}
			if e = reply(m, plist(map[string]any{"eventPort": eventListener.Addr().(*net.TCPAddr).Port})); e != nil {
				return e
			}
			event, e := eventListener.Accept()
			if e != nil {
				return e
			}
			defer event.Close()
			go func() {
				_ = event.SetDeadline(time.Now().Add(5 * time.Second))
				w := newSecure(event, secret, true)
				w.read, w.write = w.write, w.read
				err := writeAll(w, []byte("POST /command RTSP/1.0\r\nCSeq: 7\r\nContent-Length: 0\r\n\r\n"))
				if err == nil {
					var got message
					got, err = readMessage(bufio.NewReader(w))
					if err == nil && (got.line != "RTSP/1.0 200 OK" || got.header.Get("CSeq") != "7") {
						err = errors.New("bad event acknowledgement")
					}
				}
				eventDone <- err
			}()
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			if !strings.HasPrefix(m.line, "RECORD ") {
				return errors.New("RECORD must precede stream SETUP")
			}
			if e = reply(m, nil); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			value, e = unplist(m.body)
			if e != nil {
				return e
			}
			stream := value.(map[string]any)["streams"].([]any)[0].(map[string]any)
			if number(stream["type"]) != 96 || number(stream["ct"]) != 2 || number(stream["audioFormat"]) != 0x40000 {
				return errors.New("incorrect audio negotiation")
			}
			audioKey <- stream["shk"].([]byte)
			senderPort <- int(number(stream["controlPort"]))
			if e = reply(m, plist(map[string]any{"streams": []any{map[string]any{"controlPort": controlUDP.LocalAddr().(*net.UDPAddr).Port, "dataPort": data.LocalAddr().(*net.UDPAddr).Port}}})); e != nil {
				return e
			}
			for {
				m, e = readMessage(reader)
				if e != nil {
					if errors.Is(e, io.EOF) {
						return nil
					}
					return e
				}
				if e = reply(m, nil); e != nil {
					return e
				}
			}
		}()
	}()
	device := Device{Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Features: 1 << 48}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, e := Connect(ctx, device, "AABBCCDDEEFF0011", nil)
	if e != nil {
		select {
		case se := <-serverDone:
			t.Fatalf("connect: %v (receiver %v)", e, se)
		default:
			t.Fatal(e)
		}
	}
	defer session.Close()
	if e = session.Volume(35); e != nil {
		t.Fatal(e)
	}
	if e = session.Metadata(TrackMetadata{ID: "track-1", Title: "音乐", Artist: "Artist", Album: "Album", Duration: 180 * time.Second}); e != nil {
		t.Fatal(e)
	}
	key := <-audioKey
	port := <-senderPort
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- session.Stream(ctx, bytes.NewReader(make([]byte, PCMBytes*3)), func(time.Duration) {})
	}()
	_ = data.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 4096)
	count, _, e := data.ReadFromUDP(b)
	if e != nil {
		t.Fatal(e)
	}
	packet := append([]byte{}, b[:count]...)
	nonce := make([]byte, 12)
	copy(nonce[4:], packet[len(packet)-8:])
	if _, e = aead(key).Open(nil, nonce, packet[12:len(packet)-8], packet[4:12]); e != nil {
		t.Fatal("audio payload", e)
	}
	request := []byte{0x80, 0xd5, 0, 1, packet[2], packet[3], 0, 1}
	_, _ = controlUDP.WriteToUDP(request, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	_ = controlUDP.SetReadDeadline(time.Now().Add(time.Second))
	for {
		n, _, e := controlUDP.ReadFromUDP(b)
		if e != nil {
			t.Fatal(e)
		}
		if b[1] == 0xd6 {
			if !bytes.Equal(b[4:n], packet) {
				t.Fatal("retransmit differs")
			}
			break
		}
	}
	if e = <-streamDone; e != nil {
		t.Fatal(e)
	}
	if e = <-eventDone; e != nil {
		t.Fatal(e)
	}
	session.Close()
	if e = <-serverDone; e != nil {
		t.Fatal(e)
	}
}
