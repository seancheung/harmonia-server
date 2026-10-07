package airplay

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPersistentPairingAndVerify(t *testing.T) {
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	peerPublic, peerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	const peerID = "receiver-identity"
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			conn, e := listener.Accept()
			if e != nil {
				return e
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
			reader := bufio.NewReader(conn)
			reply := func(m message, body []byte) error {
				return writeAll(conn, append([]byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %s\r\nContent-Length: %d\r\n\r\n", m.header.Get("CSeq"), len(body))), body...))
			}
			for _, path := range []string{"GET /info ", "POST /pair-pin-start "} {
				m, e := readMessage(reader)
				if e != nil {
					return e
				}
				if !strings.HasPrefix(m.line, path) {
					return fmt.Errorf("expected %s, got %s", path, m.line)
				}
				if e = reply(m, nil); e != nil {
					return e
				}
			}
			m, e := readMessage(reader)
			if e != nil {
				return e
			}
			if m.header.Get("X-Apple-HKP") != "3" {
				return errors.New("incorrect normal pairing mode")
			}
			n, _ := new(big.Int).SetString(srpPrime, 16)
			g := big.NewInt(5)
			salt := []byte("normal-pair-salt!")
			b := big.NewInt(93871234)
			v := new(big.Int).Exp(g, integer(hash(salt, hash([]byte("Pair-Setup:1234")))), n)
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
			f, e := untlv(m.body)
			if e != nil {
				return e
			}
			A := integer(f[3])
			u := integer(hash(pad(A), pad(B)))
			S := new(big.Int).Mul(A, new(big.Int).Exp(v, u, n))
			S.Exp(S, b, n)
			secret := hash(S.Bytes())
			hn, hg := hash(n.Bytes()), hash(g.Bytes())
			for i := range hn {
				hn[i] ^= hg[i]
			}
			proof := hash(hn, hash([]byte("Pair-Setup")), salt, A.Bytes(), B.Bytes(), secret)
			if !bytes.Equal(proof, f[4]) {
				return errors.New("bad SRP proof")
			}
			if e = reply(m, tlv(tag(6), tag(4), tag(4), hash(A.Bytes(), proof, secret))); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			f, e = untlv(m.body)
			if e != nil {
				return e
			}
			key := derive(secret, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
			plain, e := aead(key).Open(nil, labelNonce("PS-Msg05"), f[5], nil)
			if e != nil {
				return e
			}
			client, e := untlv(plain)
			if e != nil {
				return e
			}
			signing := derive(secret, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
			signing = append(append(signing, client[1]...), client[3]...)
			if len(client[3]) != 32 || !ed25519.Verify(client[3], signing, client[10]) {
				return errors.New("bad controller identity signature")
			}
			signing = derive(secret, "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
			signing = append(append(signing, []byte(peerID)...), peerPublic...)
			sub := tlv(tag(1), []byte(peerID), tag(3), peerPublic, tag(10), ed25519.Sign(peerPrivate, signing))
			sealed := aead(key).Seal(nil, labelNonce("PS-Msg06"), sub, nil)
			if e = reply(m, tlv(tag(6), tag(6), tag(5), sealed)); e != nil {
				return e
			}
			conn.Close()
			// Fresh connection verifies the saved identity using signed X25519 keys.
			conn, e = listener.Accept()
			if e != nil {
				return e
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
			reader = bufio.NewReader(conn)
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			if e = reply(m, nil); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			if !strings.HasPrefix(m.line, "POST /pair-verify ") {
				return errors.New("expected pair-verify")
			}
			f, e = untlv(m.body)
			if e != nil {
				return e
			}
			eph, e := ecdh.X25519().GenerateKey(rand.Reader)
			if e != nil {
				return e
			}
			clientEphemeral, e := ecdh.X25519().NewPublicKey(f[3])
			if e != nil {
				return e
			}
			shared, e := eph.ECDH(clientEphemeral)
			if e != nil {
				return e
			}
			key = derive(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
			signing = append(append(eph.PublicKey().Bytes(), []byte(peerID)...), f[3]...)
			sub = tlv(tag(1), []byte(peerID), tag(10), ed25519.Sign(peerPrivate, signing))
			sealed = aead(key).Seal(nil, labelNonce("PV-Msg02"), sub, nil)
			if e = reply(m, tlv(tag(6), tag(2), tag(3), eph.PublicKey().Bytes(), tag(5), sealed)); e != nil {
				return e
			}
			m, e = readMessage(reader)
			if e != nil {
				return e
			}
			f, e = untlv(m.body)
			if e != nil {
				return e
			}
			plain, e = aead(key).Open(nil, labelNonce("PV-Msg03"), f[5], nil)
			if e != nil {
				return e
			}
			subFields, e := untlv(plain)
			if e != nil {
				return e
			}
			signing = append(append(clientEphemeral.Bytes(), client[1]...), eph.PublicKey().Bytes()...)
			if !bytes.Equal(subFields[1], client[1]) || !ed25519.Verify(client[3], signing, subFields[10]) {
				return errors.New("bad pair-verify identity")
			}
			return reply(m, tlv(tag(6), tag(4)))
		}()
	}()
	d := Device{Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	p, e := BeginPair(d, "AABBCCDDEEFF0011")
	if e != nil {
		t.Fatal(e)
	}
	cred, e := p.Finish("1234")
	if e != nil {
		t.Fatal(e)
	}
	if cred.PeerID != peerID || !bytes.Equal(cred.PeerPublic, peerPublic) || len(cred.Private) != 64 {
		t.Fatal("incorrect stored credentials")
	}
	c, e := dial(d, cred.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer c.conn.Close()
	secret, e := authenticate(c, &cred)
	if e != nil || len(secret) != 32 {
		t.Fatal("pair-verify", e)
	}
	if e = <-serverDone; e != nil && !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
}
