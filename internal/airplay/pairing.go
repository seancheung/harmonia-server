package airplay

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

type Credentials struct {
	ID         string `json:"id"`
	Private    []byte `json:"private"`
	PeerID     string `json:"peerId"`
	PeerPublic []byte `json:"peerPublic"`
}
type Pairing struct {
	ctrl         *control
	salt, public []byte
}

func (p *Pairing) Close() { p.ctrl.conn.Close() }
func BeginPair(device Device, id string) (*Pairing, error) {
	c, e := dial(device, id)
	if e != nil {
		return nil, e
	}
	// Some receivers only display the PIN after this explicit request. Receivers
	// without that endpoint can initiate PIN display from HAP M1 itself.
	response, startErr := c.request("POST", "/pair-pin-start", "", nil, nil)
	if startErr != nil && !strings.Contains(response.line, " 404 ") {
		c.conn.Close()
		return nil, startErr
	}
	t, e := c.pair("/pair-setup", tlv(tag(6), tag(1), tag(0), tag(0)), 2, 3)
	if e != nil {
		c.conn.Close()
		return nil, e
	}
	return &Pairing{c, t[2], t[3]}, nil
}
func (p *Pairing) Finish(pin string) (Credentials, error) {
	defer p.Close()
	var cred Credentials
	if len(pin) < 4 || len(pin) > 64 {
		return cred, errors.New("invalid pairing PIN")
	}
	s, e := newSRP(p.salt, p.public, pin)
	if e != nil {
		return cred, e
	}
	t, e := p.ctrl.pair("/pair-setup", tlv(tag(6), tag(3), tag(3), s.public, tag(4), s.proof), 4, 3)
	if e != nil {
		return cred, e
	}
	if e = s.verify(t[4]); e != nil {
		return cred, e
	}
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return cred, e
	}
	id := p.ctrl.id
	signing := derive(s.key, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
	msg := append(append(signing, []byte(id)...), pub...)
	sub := tlv(tag(1), []byte(id), tag(3), pub, tag(10), ed25519.Sign(priv, msg))
	key := derive(s.key, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
	sealed := aead(key).Seal(nil, labelNonce("PS-Msg05"), sub, nil)
	t, e = p.ctrl.pair("/pair-setup", tlv(tag(6), tag(5), tag(5), sealed), 6, 3)
	if e != nil {
		return cred, e
	}
	plain, e := aead(key).Open(nil, labelNonce("PS-Msg06"), t[5], nil)
	if e != nil {
		return cred, errors.New("pairing response authentication failed")
	}
	inner, e := untlv(plain)
	if e != nil {
		return cred, e
	}
	if len(inner[3]) != 32 || len(inner[1]) == 0 {
		return cred, errors.New("invalid receiver identity")
	}
	signed := derive(s.key, "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
	signed = append(append(signed, inner[1]...), inner[3]...)
	if !ed25519.Verify(inner[3], signed, inner[10]) {
		return cred, errors.New("pairing receiver signature mismatch")
	}
	return Credentials{ID: id, Private: priv, PeerID: string(inner[1]), PeerPublic: inner[3]}, nil
}
func authenticate(c *control, cred *Credentials) ([]byte, error) {
	if cred == nil {
		t, e := c.pair("/pair-setup", tlv(tag(6), tag(1), tag(0), tag(0), tag(19), []byte{0x10, 0, 0, 0}), 2, 4)
		if e != nil {
			return nil, e
		}
		s, e := newSRP(t[2], t[3], "3939")
		if e != nil {
			return nil, e
		}
		t, e = c.pair("/pair-setup", tlv(tag(6), tag(3), tag(3), s.public, tag(4), s.proof), 4, 4)
		if e != nil {
			return nil, e
		}
		if e = s.verify(t[4]); e != nil {
			return nil, e
		}
		return s.key, nil
	}
	if len(cred.Private) != 64 || len(cred.PeerPublic) != 32 || cred.ID != c.id {
		return nil, errors.New("invalid stored AirPlay credentials")
	}
	private, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}
	public := private.PublicKey().Bytes()
	t, e := c.pair("/pair-verify", tlv(tag(6), tag(1), tag(3), public), 2, 3)
	if e != nil {
		return nil, e
	}
	peer, e := ecdh.X25519().NewPublicKey(t[3])
	if e != nil {
		return nil, e
	}
	secret, e := private.ECDH(peer)
	if e != nil {
		return nil, e
	}
	key := derive(secret, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	plain, e := aead(key).Open(nil, labelNonce("PV-Msg02"), t[5], nil)
	if e != nil {
		return nil, errors.New("pair-verify authentication failed")
	}
	sub, e := untlv(plain)
	if e != nil {
		return nil, e
	}
	signed := append(append(append([]byte{}, t[3]...), sub[1]...), public...)
	if string(sub[1]) != cred.PeerID || !ed25519.Verify(cred.PeerPublic, signed, sub[10]) {
		return nil, errors.New("pair-verify receiver identity mismatch")
	}
	signed = append(append(append([]byte{}, public...), []byte(cred.ID)...), t[3]...)
	encrypted := aead(key).Seal(nil, labelNonce("PV-Msg03"), tlv(tag(1), []byte(cred.ID), tag(10), ed25519.Sign(cred.Private, signed)), nil)
	_, e = c.pair("/pair-verify", tlv(tag(6), tag(3), tag(5), encrypted), 4, 3)
	return secret, e
}
func Identity() string { b := make([]byte, 8); _, _ = rand.Read(b); return hex.EncodeToString(b) }
