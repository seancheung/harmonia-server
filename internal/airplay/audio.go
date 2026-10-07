package airplay

import (
	"encoding/binary"
	"errors"
	"time"
)

const SampleRate = 44100
const Frames = 352
const PCMBytes = Frames * 4

// ALAC's verbatim stereo element avoids a second lossy encode. This is an
// ALAC bitstream, not PCM mislabeled as ALAC. Input is signed 16-bit LE stereo.
func alac(pcm []byte) []byte {
	out := make([]byte, (23+Frames*32+3+7)/8)
	bit := 0
	put := func(value uint32, n int) {
		for j := n - 1; j >= 0; j-- {
			out[bit/8] |= byte((value>>uint(j))&1) << uint(7-bit%8)
			bit++
		}
	}
	put(1, 3)
	put(0, 4)
	put(0, 12)
	put(0, 1)
	put(0, 2)
	put(1, 1)
	for i := 0; i < Frames*2; i++ {
		put(uint32(binary.LittleEndian.Uint16(pcm[i*2:])), 16)
	}
	put(7, 3)
	return out
}
func ntp(t time.Time) uint64 {
	return uint64(t.Unix()+2208988800)<<32 | uint64(t.Nanosecond())*(1<<32)/1e9
}
func audioPacket(key []byte, seq uint16, stamp, ssrc uint32, counter uint64, pcm []byte) ([]byte, error) {
	if len(pcm) != PCMBytes {
		return nil, errors.New("invalid PCM frame size")
	}
	b := make([]byte, 12)
	b[0] = 0x80
	b[1] = 0x60
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], stamp)
	binary.BigEndian.PutUint32(b[8:], ssrc)
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], counter)
	// A monotonic 64-bit nonce is independent of the wrapping RTP sequence.
	// Reusing a nonce at the 16-bit sequence wrap would break AEAD security.
	b = aead(key).Seal(b, nonce, alac(pcm), b[4:12])
	return append(b, nonce[4:]...), nil
}
