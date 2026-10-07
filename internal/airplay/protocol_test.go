package airplay

import (
	"bufio"
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"math/big"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestBinaryPlistInteroperability(t *testing.T) {
	// Python plistlib binary fixture, independent of our encoder.
	known, _ := hex.DecodeString("62706c6973743030d1010251781001080b0d000000000000010100000000000000030000000000000000000000000000000f")
	v, e := unplist(known)
	if e != nil || !reflect.DeepEqual(v, map[string]any{"x": uint64(1)}) {
		t.Fatalf("external plist: %#v %v", v, e)
	}
	value := map[string]any{"name": "音乐🎵", "blob": []byte{1, 2, 3}, "streams": []any{map[string]any{"controlPort": uint64(5001), "dataPort": uint64(5002)}}, "long": string(bytes.Repeat([]byte{'a'}, 300)), "enabled": true}
	got, e := unplist(plist(value))
	if e != nil || !reflect.DeepEqual(got, value) {
		t.Fatalf("roundtrip: %#v %v", got, e)
	}
	for _, b := range [][]byte{{}, []byte("bplist00"), bytes.Repeat([]byte{255}, 40)} {
		if _, e := unplist(b); e == nil {
			t.Fatal("accepted malformed plist")
		}
	}
	b := plist(value)
	b[len(b)-1] = 255
	if _, e := unplist(b); e == nil {
		t.Fatal("accepted invalid offset table")
	}
}
func TestTLVFragmentationAndErrors(t *testing.T) {
	value := bytes.Repeat([]byte{0xa5}, 600)
	v, e := untlv(tlv(tag(6), tag(2), tag(3), value))
	if e != nil || !bytes.Equal(v[3], value) {
		t.Fatal("fragmented TLV", e)
	}
	for _, b := range [][]byte{{3, 4, 1}, {7, 1, 2}} {
		if _, e := untlv(b); e == nil {
			t.Fatal("accepted bad TLV")
		}
	}
}
func TestControlFramesAuthenticationAndChunking(t *testing.T) {
	secret := []byte("independent shared session secret")
	var wire bytes.Buffer
	sender := newSecure(&wire, secret, false)
	payload := bytes.Repeat([]byte("control"), 800)
	if _, e := sender.Write(payload); e != nil {
		t.Fatal(e)
	}
	// The receiver reads with the controller-write key, not controller-read.
	receiver := newSecure(&wire, secret, false)
	receiver.read = sender.write
	got := make([]byte, len(payload))
	if _, e := io.ReadFull(receiver, got); e != nil || !bytes.Equal(got, payload) {
		t.Fatal("encrypted framing", e)
	}
	wire.Reset()
	sender = newSecure(&wire, secret, false)
	_, _ = sender.Write([]byte("private"))
	data := wire.Bytes()
	data[len(data)-1] ^= 1
	receiver = newSecure(&wire, secret, false)
	receiver.read = sender.write
	if _, e := receiver.Read(got); e == nil {
		t.Fatal("accepted forged authentication tag")
	}
}
func TestSRPClientAgainstServerEquation(t *testing.T) {
	n, _ := new(big.Int).SetString(srpPrime, 16)
	g := big.NewInt(5)
	salt := []byte("0123456789abcdef")
	private := big.NewInt(0x987654321)
	pin := "1234"
	// Server calculation is separate from the sender's (A * v^u)^b mod N.
	digest := func(parts ...[]byte) []byte {
		h := sha512.New()
		for _, p := range parts {
			_, _ = h.Write(p)
		}
		return h.Sum(nil)
	}
	x := new(big.Int).SetBytes(digest(salt, digest([]byte("Pair-Setup:"+pin))))
	v := new(big.Int).Exp(g, x, n)
	k := new(big.Int).SetBytes(digest(pad(n), pad(g)))
	B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(g, private, n))
	B.Mod(B, n)
	client, e := newSRP(salt, pad(B), pin)
	if e != nil {
		t.Fatal(e)
	}
	A := new(big.Int).SetBytes(client.public)
	u := new(big.Int).SetBytes(digest(pad(A), pad(B)))
	S := new(big.Int).Mul(A, new(big.Int).Exp(v, u, n))
	S.Exp(S, private, n)
	key := digest(S.Bytes())
	if !bytes.Equal(key, client.key) {
		t.Fatal("SRP shared secret differs")
	}
	hn, hg := digest(n.Bytes()), digest(g.Bytes())
	for i := range hn {
		hn[i] ^= hg[i]
	}
	proof := digest(hn, digest([]byte("Pair-Setup")), salt, A.Bytes(), B.Bytes(), key)
	if !bytes.Equal(proof, client.proof) {
		t.Fatal("SRP controller proof differs")
	}
	if e = client.verify(digest(A.Bytes(), proof, key)); e != nil {
		t.Fatal(e)
	}
	if e = client.verify(make([]byte, 64)); e == nil {
		t.Fatal("accepted invalid receiver proof")
	}
	if _, e = newSRP(salt, make([]byte, 384), pin); e == nil {
		t.Fatal("accepted zero public key")
	}
	if _, e = newSRP(salt, pad(n), pin); e == nil {
		t.Fatal("accepted public key N")
	}
}
func TestAudioPacketAndNonceWrap(t *testing.T) {
	pcm := make([]byte, PCMBytes)
	for i := 0; i < Frames*2; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(i*97))
	}
	key := bytes.Repeat([]byte{42}, 32)
	first, e := audioPacket(key, 0, 1000, 22, 0, pcm)
	if e != nil {
		t.Fatal(e)
	}
	wrapped, e := audioPacket(key, 0, 1000, 22, 65536, pcm)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Equal(first, wrapped) {
		t.Fatal("audio nonce reused at RTP sequence wrap")
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], first[len(first)-8:])
	raw, e := aead(key).Open(nil, nonce, first[12:len(first)-8], first[4:12])
	if e != nil {
		t.Fatal(e)
	}
	bit := 0
	get := func(n int) uint32 {
		var v uint32
		for i := 0; i < n; i++ {
			v = v<<1 | uint32((raw[bit/8]>>uint(7-bit%8))&1)
			bit++
		}
		return v
	}
	if get(3) != 1 || get(4) != 0 || get(12) != 0 || get(1) != 0 || get(2) != 0 || get(1) != 1 {
		t.Fatal("invalid ALAC element header")
	}
	for i := 0; i < Frames*2; i++ {
		if get(16) != uint32(binary.LittleEndian.Uint16(pcm[2*i:])) {
			t.Fatalf("ALAC sample %d differs", i)
		}
	}
	if get(3) != 7 {
		t.Fatal("missing ALAC end element")
	}
	first[4] ^= 1
	if _, e = aead(key).Open(nil, nonce, first[12:len(first)-8], first[4:12]); e == nil {
		t.Fatal("unauthenticated RTP timestamp")
	}
}
func TestNetworkParsersRejectMalformedInput(t *testing.T) {
	for _, b := range [][]byte{{0xc0, 0}, {63, 1}, {0xff, 0xff}} {
		if _, _, e := dnsName(b, 0); e == nil {
			t.Fatal("accepted malformed DNS")
		}
	}
	for _, s := range []string{"RTSP/1.0 200 OK\r\nContent-Length: -1\r\n\r\n", "RTSP/1.0 200 OK\r\nContent-Length: 4\r\nContent-Length: 4\r\n\r\nxxxx"} {
		if _, e := readMessage(bufio.NewReader(bytes.NewBufferString(s))); e == nil {
			t.Fatal("accepted invalid RTSP")
		}
	}
	if featureBits("0x0,0x10000") != 1<<48 {
		t.Fatal("incorrect feature word order")
	}
	if ntp(time.Unix(0, 500000000)) != uint64(2208988800)<<32|1<<31 {
		t.Fatal("incorrect NTP epoch/fraction")
	}
}
func FuzzPlist(f *testing.F) {
	f.Add(plist(map[string]any{"streams": []any{uint64(96)}}))
	f.Add([]byte("bplist00"))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = unplist(b) })
}
func FuzzDiscovery(f *testing.F) {
	f.Add(query("_airplay._tcp.local.", 12))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = dnsRecords(b) })
}

func TestALACDecodesWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg unavailable")
	}
	pcm := make([]byte, PCMBytes)
	for i := 0; i < Frames*2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i*107-32000))
	}
	packet := alac(pcm)
	// CAF wrapper supplies the ALAC cookie; FFmpeg independently decodes the
	// elementary frame produced by our encoder, including signed stereo samples.
	file := []byte{'c', 'a', 'f', 'f', 0, 1, 0, 0}
	chunk := func(name string, b []byte) {
		file = append(file, []byte(name)...)
		file = binary.BigEndian.AppendUint64(file, uint64(len(b)))
		file = append(file, b...)
	}
	desc := binary.BigEndian.AppendUint64(nil, math.Float64bits(44100))
	desc = append(desc, []byte("alac")...)
	for _, v := range []uint32{0, uint32(len(packet)), Frames, 2, 0} {
		desc = binary.BigEndian.AppendUint32(desc, v)
	}
	chunk("desc", desc)
	cookie, _ := hex.DecodeString("0000000c66726d61616c616300000024616c616300000000000010000010280a0e02000000004004001588800000ac44")
	binary.BigEndian.PutUint32(cookie[24:], Frames)
	binary.BigEndian.PutUint32(cookie[36:], uint32(len(packet)))
	chunk("kuki", cookie)
	chunk("data", append(make([]byte, 4), packet...))
	pakt := make([]byte, 24)
	binary.BigEndian.PutUint64(pakt, 1)
	binary.BigEndian.PutUint64(pakt[8:], Frames)
	chunk("pakt", pakt)
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "caf", "-i", "pipe:0", "-f", "s16le", "-c:a", "pcm_s16le", "pipe:1")
	cmd.Stdin = bytes.NewReader(file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	decoded, err := cmd.Output()
	if err != nil {
		t.Fatalf("ALAC decode: %v %s", err, stderr.String())
	}
	if !bytes.Equal(decoded, pcm) {
		t.Fatalf("ALAC PCM differs: got %d bytes, expected %d", len(decoded), len(pcm))
	}
}
