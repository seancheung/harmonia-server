package airplay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRAOPDiscovery(t *testing.T) {
	hosts := map[string]string{"speaker.local.": "192.0.2.1"}
	legacy := &discoveredService{name: "AABBCCDDEEFF@Speaker", host: "speaker.local.", port: 5000, txt: map[string]string{"cn": "0,1", "et": "0,1", "tp": "UDP"}}
	services := map[string]*discoveredService{"aabbccddeeff@speaker._raop._tcp.local.": legacy}
	got := devicesFromServices(services, hosts)
	if len(got) != 1 || got[0].ID != "aa:bb:cc:dd:ee:ff" || got[0].Name != "Speaker" || got[0].Type != "AirPlay 1" || !got[0].RSA || got[0].UnsupportedReason != "" {
		t.Fatalf("%+v", got)
	}
	services["speaker._airplay._tcp.local."] = &discoveredService{name: "Speaker", host: "speaker.local.", port: 7000, txt: map[string]string{"deviceid": "AA:BB:CC:DD:EE:FF", "features": "0x0,0x10000"}}
	for range 30 {
		got = devicesFromServices(services, hosts)
		if len(got) != 1 || got[0].Type != "AirPlay 2" || got[0].Port != 7000 {
			t.Fatalf("%+v", got)
		}
	}
	delete(services, "speaker._airplay._tcp.local.")
	for _, bad := range []map[string]string{{"cn": "0"}, {"tp": "TCP"}, {"et": "3,5"}, {"pw": "true"}} {
		legacy.txt = bad
		got = devicesFromServices(services, hosts)
		if got[0].UnsupportedReason == "" {
			t.Fatalf("accepted %v", bad)
		}
	}
}
func TestRAOPTransport(t *testing.T) {
	p, e := raopTransport(`RTP/AVP/UDP;unicast;server_port=1234;control_port="2345";timing_port=3456`)
	if e != nil || p["server_port"] != 1234 || p["control_port"] != 2345 {
		t.Fatal(p, e)
	}
	for _, bad := range []string{"", "RTP/AVP/TCP;server_port=1;control_port=2", "RTP/AVP/UDP;server_port=0;control_port=2", "RTP/AVP/UDP;server_port=65536;control_port=2", "RTP/AVP/UDP;server_port=1"} {
		if _, e := raopTransport(bad); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestRAOPAudioEncryption(t *testing.T) {
	pcm := make([]byte, PCMBytes)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	key, iv := bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{7}, 16)
	plain, e := raopPacket(nil, nil, 65535, 123, 456, pcm)
	if e != nil {
		t.Fatal(e)
	}
	encrypted, e := raopPacket(key, iv, 65535, 123, 456, pcm)
	if e != nil {
		t.Fatal(e)
	}
	if binary.BigEndian.Uint16(plain[2:]) != 65535 || binary.BigEndian.Uint32(plain[4:]) != 123 || !bytes.Equal(plain[12:], alac(pcm)) {
		t.Fatal("incorrect RTP/ALAC")
	}
	full := (len(plain) - 12) / 16 * 16
	if bytes.Equal(plain[12:12+full], encrypted[12:12+full]) || !bytes.Equal(plain[12+full:], encrypted[12+full:]) {
		t.Fatal("incorrect CBC extent")
	}
	again, _ := raopPacket(key, iv, 65535, 123, 456, pcm)
	if !bytes.Equal(again, encrypted) {
		t.Fatal("IV was not reset")
	}
	block, _ := aes.NewCipher(key)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(encrypted[12:12+full], encrypted[12:12+full])
	if !bytes.Equal(plain, encrypted) {
		t.Fatal("CBC round trip")
	}
}

// Independent RTSP receiver checks that RAOP never enters HAP, that Session is
// retained, and that a volume-only connection never starts recording.
func TestRAOPSession(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(strconv.FormatBool(record), func(t *testing.T) {
			listener, e := net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer listener.Close()
			audio, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if e != nil {
				t.Fatal(e)
			}
			defer audio.Close()
			control, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if e != nil {
				t.Fatal(e)
			}
			defer control.Close()
			done := make(chan error, 1)
			go func() {
				done <- func() error {
					conn, e := listener.Accept()
					if e != nil {
						return e
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(10 * time.Second))
					r := bufio.NewReader(conn)
					expected := []string{"OPTIONS", "ANNOUNCE", "SETUP"}
					if record {
						expected = append(expected, "RECORD", "SET_PARAMETER", "SET_PARAMETER", "TEARDOWN")
					} else {
						expected = append(expected, "GET_PARAMETER", "TEARDOWN")
					}
					for i, method := range expected {
						m, e := readMessage(r)
						if e != nil {
							return e
						}
						if !strings.HasPrefix(m.line, method+" ") {
							return fmt.Errorf("expected %s got %s", method, m.line)
						}
						if i > 2 && m.header.Get("Session") != "opaque-session" {
							return fmt.Errorf("missing Session")
						}
						headers, body := "", ""
						switch method {
						case "ANNOUNCE":
							if m.header.Get("Content-Type") != "application/sdp" || !bytes.Contains(m.body, []byte("a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100")) {
								return fmt.Errorf("bad SDP")
							}
						case "SETUP":
							headers = fmt.Sprintf("Session: opaque-session;timeout=60\r\nTransport: RTP/AVP/UDP;server_port=%d;control_port=%d\r\n", audio.LocalAddr().(*net.UDPAddr).Port, control.LocalAddr().(*net.UDPAddr).Port)
							timing := 0
							for _, p := range strings.Split(m.header.Get("Transport"), ";") {
								if strings.HasPrefix(p, "timing_port=") {
									timing, _ = strconv.Atoi(strings.TrimPrefix(p, "timing_port="))
								}
							}
							if timing == 0 {
								return fmt.Errorf("no timing port")
							}
							probe := make([]byte, 32)
							probe[0] = 0x80
							probe[1] = 0xd2
							if _, e = control.WriteToUDP(probe, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: timing}); e != nil {
								return e
							}
						case "RECORD":
							if m.header.Get("RTP-Info") == "" {
								return fmt.Errorf("missing RTP origin")
							}
						case "GET_PARAMETER":
							body = "volume: -15.000000\r\n"
						}
						if e = writeAll(conn, []byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s", m.header.Get("CSeq"), headers, len(body), body))); e != nil {
							return e
						}
					}
					return nil
				}()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			d := Device{Type: "AirPlay 1", Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
			if !record {
				v, e := ReadVolume(ctx, d, Identity(), nil)
				if e != nil || v != 50 {
					t.Fatalf("volume %d: %v", v, e)
				}
			} else {
				s, e := Connect(ctx, d, Identity(), nil)
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				if e = s.Metadata(TrackMetadata{Title: "Song", Artist: "Artist"}); e != nil {
					t.Fatal(e)
				}
				if e = s.Artwork(nil); e != nil {
					t.Fatal(e)
				}
				pcm := make([]byte, PCMBytes)
				pcm[0] = 123
				if e = s.Stream(ctx, bytes.NewReader(pcm), func(time.Duration) {}); e != nil {
					t.Fatal(e)
				}
				audio.SetReadDeadline(time.Now().Add(time.Second))
				packet := make([]byte, 2048)
				n, _, e := audio.ReadFromUDP(packet)
				if e != nil {
					t.Fatal(e)
				}
				if packet[1] != 0xe0 || !bytes.Equal(packet[12:n], alac(pcm)) {
					t.Fatal("incorrect audio datagram")
				}
				s.Close()
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
		})
	}
}
