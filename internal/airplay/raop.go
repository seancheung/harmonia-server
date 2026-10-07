package airplay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"
)

func connectRAOP(ctx context.Context, device Device, id string, record bool) (*Session, error) {
	if device.UnsupportedReason != "" {
		return nil, errors.New(device.UnsupportedReason)
	}
	c, err := dial(device, id)
	if err != nil {
		return nil, err
	}
	s := &Session{legacy: true, ctrl: c, done: make(chan struct{}), packets: map[uint16][]byte{}, lead: 2 * time.Second}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	if _, err = c.request("OPTIONS", "*", "", nil, nil); err != nil {
		return nil, err
	}
	local := c.conn.LocalAddr().(*net.TCPAddr).IP
	peer := net.ParseIP(device.Address)
	s.clock, err = startClock(local, peer, 0, false)
	if err != nil {
		return nil, err
	}
	for _, socket := range []**net.UDPConn{&s.data, &s.rtcp} {
		*socket, err = net.ListenUDP("udp4", &net.UDPAddr{IP: local})
		if err != nil {
			return nil, err
		}
	}
	var random [10]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	s.seq = binary.BigEndian.Uint16(random[:2])
	s.stamp = binary.BigEndian.Uint32(random[2:6])
	s.ssrc = binary.BigEndian.Uint32(random[6:])
	sdp := fmt.Sprintf("v=0\r\no=Harmonia %d 0 IN IP4 %s\r\ns=Harmonia\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio 0 RTP/AVP 96\r\na=rtpmap:96 AppleLossless\r\na=fmtp:96 %d 0 16 40 10 14 2 255 0 0 %d\r\n", s.ssrc, local.String(), device.Address, Frames, SampleRate)
	if device.RSA {
		s.aesKey = make([]byte, 16)
		s.aesIV = make([]byte, 16)
		if _, err = rand.Read(s.aesKey); err != nil {
			return nil, err
		}
		if _, err = rand.Read(s.aesIV); err != nil {
			return nil, err
		}
		key, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, raopPublicKey(), s.aesKey, nil)
		if err != nil {
			return nil, err
		}
		sdp += "a=rsaaeskey:" + base64.RawStdEncoding.EncodeToString(key) + "\r\na=aesiv:" + base64.RawStdEncoding.EncodeToString(s.aesIV) + "\r\n"
	}
	if _, err = c.request("ANNOUNCE", "", "application/sdp", []byte(sdp), nil); err != nil {
		return nil, err
	}
	transport := fmt.Sprintf("RTP/AVP/UDP;unicast;mode=record;control_port=%d;timing_port=%d", s.rtcp.LocalAddr().(*net.UDPAddr).Port, s.clock.event.LocalAddr().(*net.UDPAddr).Port)
	response, err := c.request("SETUP", "", "", nil, map[string]string{"Transport": transport})
	if err != nil {
		return nil, err
	}
	s.active.Store(true)
	c.session = strings.TrimSpace(strings.Split(response.header.Get("Session"), ";")[0])
	ports, err := raopTransport(response.header.Get("Transport"))
	if err != nil {
		return nil, err
	}
	s.target = &net.UDPAddr{IP: peer, Port: ports["server_port"]}
	s.controlTarget = &net.UDPAddr{IP: peer, Port: ports["control_port"]}
	if record {
		response, err = c.request("RECORD", "", "", nil, map[string]string{"Range": "npt=0-", "RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.seq, s.stamp)})
		if err != nil {
			return nil, err
		}
		if value := response.header.Get("Audio-Latency"); value != "" {
			frames, err := strconv.ParseUint(value, 10, 32)
			if err != nil || frames > SampleRate*10 {
				return nil, errors.New("invalid RAOP audio latency")
			}
			s.lead = max(s.lead, time.Duration(frames)*time.Second/SampleRate)
		}
		s.wg.Add(2)
		go s.retransmits()
		go s.feedback()
	}
	ok = true
	return s, nil
}

func raopTransport(value string) (map[string]int, error) {
	parts := strings.Split(value, ";")
	if !strings.EqualFold(strings.TrimSpace(parts[0]), "RTP/AVP/UDP") {
		return nil, errors.New("receiver did not negotiate RAOP UDP transport")
	}
	ports := map[string]int{}
	for _, part := range parts[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if key != "server_port" && key != "control_port" && key != "timing_port" {
			continue
		}
		port, err := strconv.Atoi(strings.Trim(val, "\""))
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid RAOP UDP port")
		}
		ports[key] = port
	}
	if ports["server_port"] == 0 || ports["control_port"] == 0 {
		return nil, errors.New("missing RAOP audio/control ports")
	}
	return ports, nil
}

func raopPacket(key, iv []byte, seq uint16, stamp, ssrc uint32, pcm []byte) ([]byte, error) {
	if len(pcm) != PCMBytes {
		return nil, errors.New("invalid PCM frame size")
	}
	b := make([]byte, 12)
	b[0] = 0x80
	b[1] = 0x60
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], stamp)
	binary.BigEndian.PutUint32(b[8:], ssrc)
	payload := alac(pcm)
	if len(key) > 0 {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		if len(iv) != aes.BlockSize {
			return nil, errors.New("invalid RAOP IV")
		}
		// Each ALAC packet restarts CBC with the session IV. Only complete blocks
		// are encrypted; the trailing bytes are transmitted without padding.
		full := len(payload) / aes.BlockSize * aes.BlockSize
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(payload[:full], payload[:full])
	}
	return append(b, payload...), nil
}

// Published AirPort Express RSA public modulus; exponent 65537.
func raopPublicKey() *rsa.PublicKey {
	b, _ := hex.DecodeString("e7d744f2a2e2788b6c1f55a08eb70544a8fa7945aa8be6c62ce5f51cbdd4dc6842fe3d1083dd2edec1bfd4252dc02e6f398bdf0e6148ea84855e2e442da6d62664f674a1f304929ade4f6893ef2df6e711a8c77a0d91c9d980822e50d12922afea40ea9f0e14c0f76938c5f3882fc0323dd9fe55155f51bb5921c201629fd73352d5e2efaabf9ba048d7b813a2b6767f6c3ccf1eb4ce673d037b0d2ea30c5fffeb06f8d08adde409571a9c689fef10728855dd8cfb9a8bef5c8943ef3b5faa15dde698beddf3599603eb3e6f61372bb628f6559f599a78bf500687aa7f4976c0562d412956f8989e18a6355bd81597825e0fc875343ec782117625cdbf98447b")
	return &rsa.PublicKey{N: new(big.Int).SetBytes(b), E: 65537}
}
