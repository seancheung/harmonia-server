package airplay

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

func derive(secret []byte, salt, info string) []byte {
	k, e := hkdf.Key(sha512.New, secret, []byte(salt), info, 32)
	if e != nil {
		panic(e)
	}
	return k
}
func aead(k []byte) cipher.AEAD {
	a, e := chacha20poly1305.New(k)
	if e != nil {
		panic(e)
	}
	return a
}
func labelNonce(label string) []byte { n := make([]byte, 12); copy(n[4:], label); return n }
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

type secureWire struct {
	io.ReadWriter
	read, write cipher.AEAD
	in, out     uint64
	pending     []byte
}

func newSecure(rw io.ReadWriter, secret []byte, event bool) *secureWire {
	salt, prefix := "Control-Salt", "Control-"
	if event {
		salt, prefix = "Events-Salt", "Events-"
	}
	read, write := derive(secret, salt, prefix+"Read-Encryption-Key"), derive(secret, salt, prefix+"Write-Encryption-Key")
	if event {
		read, write = write, read
	}
	return &secureWire{ReadWriter: rw, read: aead(read), write: aead(write)}
}
func (s *secureWire) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pending) == 0 {
		h := make([]byte, 2)
		if _, e := io.ReadFull(s.ReadWriter, h); e != nil {
			return 0, e
		}
		l := int(binary.LittleEndian.Uint16(h))
		if l == 0 || l > 1024 {
			return 0, errors.New("invalid encrypted frame length")
		}
		b := make([]byte, l+16)
		if _, e := io.ReadFull(s.ReadWriter, b); e != nil {
			return 0, e
		}
		n := make([]byte, 12)
		binary.LittleEndian.PutUint64(n[4:], s.in)
		var e error
		s.pending, e = s.read.Open(nil, n, b, h)
		if e != nil {
			return 0, errors.New("AirPlay control authentication failed")
		}
		s.in++
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func (s *secureWire) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(1024, len(p))
		h := binary.LittleEndian.AppendUint16(nil, uint16(n))
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[4:], s.out)
		b := s.write.Seal(h, nonce, p[:n], h)
		if e := writeAll(s.ReadWriter, b); e != nil {
			return total, e
		}
		s.out++
		total += n
		p = p[n:]
	}
	return total, nil
}

type message struct {
	line   string
	header textproto.MIMEHeader
	body   []byte
}

func boundedLine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > 4096 {
			return "", errors.New("oversized RTSP line")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(line), err
	}
}

func readMessage(r *bufio.Reader) (message, error) {
	var m message
	line, e := boundedLine(r)
	if e != nil {
		return m, e
	}
	if len(line) > 4096 {
		return m, errors.New("oversized RTSP status")
	}
	m.line = strings.TrimSpace(line)
	m.header = make(textproto.MIMEHeader)
	total := len(line)
	for {
		line, e = boundedLine(r)
		if e != nil {
			return m, e
		}
		total += len(line)
		if total > 32768 {
			return m, errors.New("oversized RTSP headers")
		}
		if line == "\r\n" {
			break
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return m, errors.New("invalid RTSP header")
		}
		m.header.Add(k, strings.TrimSpace(v))
	}
	if values := m.header.Values("Content-Length"); len(values) > 1 {
		return m, errors.New("duplicate content length")
	}
	if v := m.header.Get("Content-Length"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 || n > 2<<20 {
			return m, errors.New("invalid RTSP body length")
		}
		m.body = make([]byte, n)
		_, e = io.ReadFull(r, m.body)
		if e != nil {
			return m, e
		}
	}
	return m, nil
}

type control struct {
	conn    net.Conn
	rw      io.ReadWriter
	reader  *bufio.Reader
	mu      sync.Mutex
	seq     int
	url, id string
	session string
	legacy  bool
}

func (c *control) encrypt(secret []byte) {
	c.rw = newSecure(structRW{c.reader, c.conn}, secret, false)
	c.reader = bufio.NewReader(c.rw)
}

type structRW struct {
	io.Reader
	io.Writer
}

var ErrPairingRequired = errors.New("AirPlay receiver requires PIN verification; open output settings to verify")

func (c *control) request(method, path, typ string, body []byte, headers map[string]string) (message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exchange(method, path, typ, body, headers, 8*time.Second)
}

// exchange requires mu and allows bounded best-effort teardown.
func (c *control) exchange(method, path, typ string, body []byte, headers map[string]string, timeout time.Duration) (message, error) {
	c.seq++
	if path == "" {
		path = c.url
	}
	_ = c.conn.SetDeadline(time.Now().Add(timeout))
	var b bytes.Buffer
	agent := "AirPlay/409.16"
	if c.legacy {
		agent = "iTunes/12.9.4 (Macintosh; OS X 10.14.4)"
	}
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: %s\r\nDACP-ID: %s\r\nClient-Instance: %s\r\n", method, path, c.seq, agent, c.id, c.id)
	if c.session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", c.session)
	}
	if typ != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", typ)
	}
	for k, v := range headers {
		if strings.ContainsAny(k+v, "\r\n") {
			return message{}, errors.New("invalid RTSP header")
		}
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	if e := writeAll(c.rw, b.Bytes()); e != nil {
		return message{}, e
	}
	m, e := readMessage(c.reader)
	if e != nil {
		return m, e
	}
	parts := strings.Fields(m.line)
	if c.legacy && len(parts) >= 2 && (parts[1] == "401" || parts[1] == "470") {
		return m, errors.New("AirPlay 1 receiver requires authentication not supported by this sender")
	}
	if len(parts) >= 2 && (parts[1] == "401" || parts[1] == "470") {
		return m, fmt.Errorf("%w: %s", ErrPairingRequired, m.line)
	}
	if len(parts) < 2 || parts[1] != "200" {
		return m, fmt.Errorf("AirPlay %s %s: %s", method, path, m.line)
	}
	if seq := m.header.Get("CSeq"); seq != "" && seq != strconv.Itoa(c.seq) {
		return m, errors.New("RTSP sequence mismatch")
	}
	return m, nil
}
func (c *control) property(method, path string, v any) (map[string]any, error) {
	m, e := c.request(method, path, "application/x-apple-binary-plist", plist(v), nil)
	if e != nil {
		return nil, e
	}
	if len(m.body) == 0 {
		return map[string]any{}, nil
	}
	value, e := unplist(m.body)
	if e != nil {
		return nil, e
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid AirPlay response dictionary")
	}
	return out, nil
}

func tlv(values ...[]byte) []byte {
	var out []byte
	for i := 0; i < len(values); i += 2 {
		tag := values[i][0]
		v := values[i+1]
		if len(v) == 0 {
			out = append(out, tag, 0)
		}
		for len(v) > 0 {
			n := min(255, len(v))
			out = append(out, tag, byte(n))
			out = append(out, v[:n]...)
			v = v[n:]
		}
	}
	return out
}
func untlv(b []byte) (map[byte][]byte, error) {
	m := map[byte][]byte{}
	for len(b) > 0 {
		if len(b) < 2 || int(b[1])+2 > len(b) {
			return nil, errors.New("invalid pairing TLV")
		}
		n := int(b[1])
		m[b[0]] = append(m[b[0]], b[2:2+n]...)
		b = b[2+n:]
	}
	if e := m[7]; len(e) > 0 {
		if e[0] == 2 {
			return nil, fmt.Errorf("%w (authentication rejected)", ErrPairingRequired)
		}
		return nil, fmt.Errorf("AirPlay pairing rejected (code %d)", e[0])
	}
	return m, nil
}
func tag(n byte) []byte { return []byte{n} }
func (c *control) pair(path string, body []byte, state, hkp byte) (map[byte][]byte, error) {
	m, e := c.request("POST", path, "application/octet-stream", body, map[string]string{"X-Apple-HKP": strconv.Itoa(int(hkp))})
	if e != nil {
		return nil, e
	}
	t, e := untlv(m.body)
	if e != nil {
		return nil, e
	}
	if !bytes.Equal(t[6], []byte{state}) {
		return nil, errors.New("unexpected pairing state")
	}
	return t, nil
}
