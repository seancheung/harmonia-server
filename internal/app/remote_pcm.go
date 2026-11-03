package app

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
)

// Both the HTTP AirPlay preview and native playback use this exact WAV format.
// Gain is baked into the cached samples once, never applied during transport.
func (a *App) remotePCM(ctx context.Context, track Track, source string, saved RemoteSaved) (io.Reader, func(), error) {
	gain := ReplayGain(track, remoteGainMode(saved), math.Max(-30, math.Min(30, saved.Preamp)), saved.Protect)
	path, release, err := a.cache.Acquire(ctx, track, source, Conversion{Codec: "wav", OutputSampleRate: 44100, OutputChannels: 2}, &gain)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		release()
		return nil, nil, err
	}
	close := func() { f.Close(); release() }
	pcm, err := wavPCM(f, saved.Position)
	if err != nil {
		close()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		close()
		return nil, nil, err
	}
	return pcm, close, nil
}

// Parse chunks instead of assuming a 44-byte WAV header: FFmpeg also writes
// LIST/JUNK chunks. Restrict reads to data, and seek on stereo sample boundaries.
func wavPCM(f *os.File, positionMS int) (io.Reader, error) {
	bad := errors.New("invalid AirPlay PCM cache (expected 44.1 kHz, stereo, 16-bit WAV)")
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, err
	}
	rf64 := string(header[:4]) == "RF64"
	if (!rf64 && string(header[:4]) != "RIFF") || string(header[8:]) != "WAVE" {
		return nil, bad
	}
	end := int64(binary.LittleEndian.Uint32(header[4:8])) + 8
	var largeData int64
	if rf64 {
		var ds [36]byte
		if _, err := f.ReadAt(ds[:], 12); err != nil {
			return nil, err
		}
		if string(ds[:4]) != "ds64" || binary.LittleEndian.Uint32(ds[4:8]) < 28 {
			return nil, bad
		}
		riffSize, dataSize := binary.LittleEndian.Uint64(ds[8:16]), binary.LittleEndian.Uint64(ds[16:24])
		if riffSize > uint64(info.Size()-8) || dataSize > uint64(info.Size()) {
			return nil, bad
		}
		end, largeData = int64(riffSize)+8, int64(dataSize)
	}
	if end > info.Size() || end < 12 {
		return nil, bad
	}
	valid := false
	for at := int64(12); at+8 <= end; {
		var chunk [8]byte
		if _, err := f.ReadAt(chunk[:], at); err != nil {
			return nil, err
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		if rf64 && string(chunk[:4]) == "data" && size == 0xffffffff {
			size = largeData
		}
		start := at + 8
		if size > end-start {
			return nil, bad
		}
		switch string(chunk[:4]) {
		case "fmt ":
			if size < 16 {
				return nil, bad
			}
			var format [16]byte
			if _, err := f.ReadAt(format[:], start); err != nil {
				return nil, err
			}
			valid = binary.LittleEndian.Uint16(format[:2]) == 1 && binary.LittleEndian.Uint16(format[2:4]) == 2 && binary.LittleEndian.Uint32(format[4:8]) == 44100 && binary.LittleEndian.Uint32(format[8:12]) == 176400 && binary.LittleEndian.Uint16(format[12:14]) == 4 && binary.LittleEndian.Uint16(format[14:16]) == 16
		case "data":
			if !valid || size == 0 || size%4 != 0 || positionMS < 0 {
				return nil, bad
			}
			if int64(positionMS) > (size/4)*1000/44100 {
				return nil, errors.New("seek position is beyond cached audio duration")
			}
			offset := int64(positionMS) * 44100 / 1000 * 4
			if offset < 0 || offset >= size {
				return nil, errors.New("seek position is beyond cached audio duration")
			}
			return io.NewSectionReader(f, start+offset, size-offset), nil
		}
		at = start + size + (size % 2)
	}
	return nil, bad
}

// Prefetch only a deterministic successor; never advance the shuffle round or
// change the queue to guess what might play next. Cancellation follows playback.
func (r *Remote) prefetchNext(ctx context.Context, generation uint64) {
	r.mu.Lock()
	saved := r.saved
	if generation != r.generation || saved.Shuffle || saved.Finish || saved.Repeat == "single" || len(saved.Queue) < 2 {
		r.mu.Unlock()
		return
	}
	next := saved.Index + 1
	if next >= len(saved.Queue) {
		if saved.Repeat != "all" {
			r.mu.Unlock()
			return
		}
		next = 0
	}
	id := saved.Queue[next]
	r.mu.Unlock()
	if r.app.store.CacheLimit() <= 0 {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		track, source, err := trackFromState(r.app.store.Read(), id)
		if err != nil {
			return
		}
		gain := ReplayGain(track, remoteGainMode(saved), math.Max(-30, math.Min(30, saved.Preamp)), saved.Protect)
		_, release, err := r.app.cache.Acquire(ctx, track, source, Conversion{Codec: "wav", OutputSampleRate: 44100, OutputChannels: 2}, &gain)
		if err == nil {
			release()
		}
	}()
}
