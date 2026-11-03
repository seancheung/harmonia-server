package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemotePCMReusesPreviewGainAndSeeksWithoutDecoder(t *testing.T) {
	a, _ := remoteFixture(t)
	track, source, err := trackFromState(a.store.Read(), "a")
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]byte, 44100*4)
	for at := 0; at < len(samples); at += 2 {
		binary.LittleEndian.PutUint16(samples[at:], 10000)
	}
	if err := os.WriteFile(source, testPCMBytes(samples), 0600); err != nil {
		t.Fatal(err)
	}
	gainDB := -6.0
	track.TrackGain = &gainDB
	saved := RemoteSaved{Gain: "track", Protect: true}
	// Seed through the HTTP preview's settings, then make FFmpeg unavailable.
	rule, gain := a.streamConversion(track, httptest.NewRequest("GET", "/?output=airplay&gain=track&preamp=0&protect=true", nil))
	path, release, err := a.cache.Acquire(context.Background(), track, source, *rule, gain)
	if err != nil {
		t.Fatal(err)
	}
	release()
	a.cache.ffmpeg = filepath.Join(t.TempDir(), "missing")
	for _, pos := range []int{0, 250, 500, 0} {
		saved.Position = pos
		pcm, close, err := a.remotePCM(context.Background(), track, source, saved)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(pcm)
		close()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != (44100-int(int64(pos)*44100/1000))*4 {
			t.Fatalf("seek %d: %d bytes", pos, len(data))
		}
		value := int16(binary.LittleEndian.Uint16(data))
		expected := 10000 * math.Pow(10, -6.0/20)
		if math.Abs(float64(value)-expected) > 1 {
			t.Fatalf("gain applied incorrectly: %d", value)
		}
	}
	// Effective gain, rather than the selected mode label, determines identity.
	saved.Gain = "album" // Falls back to the same track gain.
	pcm, close, err := a.remotePCM(context.Background(), track, source, saved)
	if err != nil {
		t.Fatal(err)
	}
	a.cache.Clear()
	if _, err := io.ReadAll(pcm); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("active cache was removed", err)
	}
	close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("released pending cache survived", err)
	}
}

func TestRemotePCMInvalidatesGainAndRevision(t *testing.T) {
	a, _ := remoteFixture(t)
	track, source, _ := trackFromState(a.store.Read(), "a")
	db := -6.0
	track.TrackGain = &db
	saved := RemoteSaved{Gain: "track", Protect: true}
	_, close, err := a.remotePCM(context.Background(), track, source, saved)
	if err != nil {
		t.Fatal(err)
	}
	close()
	a.cache.ffmpeg = filepath.Join(t.TempDir(), "missing")
	saved.Preamp = 3
	if _, close, err := a.remotePCM(context.Background(), track, source, saved); err == nil {
		close()
		t.Fatal("gain change reused old PCM")
	}
	saved.Preamp = 0
	track.Revision = "changed"
	if _, close, err := a.remotePCM(context.Background(), track, source, saved); err == nil {
		close()
		t.Fatal("revision change reused old PCM")
	}
}

func TestWAVPCMChunksAndValidation(t *testing.T) {
	data := testPCMBytes([]byte("12345678"))
	// Include an odd-sized unknown chunk and its alignment padding before fmt.
	data = append(append(append([]byte{}, data[:12]...), []byte{'J', 'U', 'N', 'K', 1, 0, 0, 0, 42, 0}...), data[12:]...)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	for _, broken := range []bool{false, true} {
		f, err := os.CreateTemp(t.TempDir(), "pcm")
		if err != nil {
			t.Fatal(err)
		}
		if broken {
			data = data[:len(data)-1]
		}
		f.Write(data)
		f.Seek(0, 0)
		pcm, err := wavPCM(f, 0)
		if broken {
			if err == nil {
				t.Fatal("accepted truncated WAV")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(pcm)
			if !bytes.Equal(got, []byte("12345678")) {
				t.Fatalf("header leaked into PCM: %q", got)
			}
			if _, err := wavPCMAtStart(f, 1); err == nil {
				t.Fatal("accepted seek past EOF")
			}
		}
		f.Close()
	}
}
func wavPCMAtStart(f *os.File, pos int) (io.Reader, error) { f.Seek(0, 0); return wavPCM(f, pos) }

func TestCacheWaitCanBeCancelled(t *testing.T) {
	a, _ := remoteFixture(t)
	track, source, _ := trackFromState(a.store.Read(), "a")
	rule := Conversion{Codec: "wav", OutputSampleRate: 44100, OutputChannels: 2}
	_, release, err := a.cache.Acquire(context.Background(), track, source, rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	var lock chan struct{}
	for _, l := range a.cache.locks {
		lock = l
	}
	<-lock
	defer func() { lock <- struct{}{} }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, release, err := a.cache.Acquire(ctx, track, source, rule, nil); err == nil {
		release()
		t.Fatal("ignored cancellation")
	}
}

func TestRemotePrefetchMakesNextTrackAvailableWithoutDecoder(t *testing.T) {
	a, sessions := remoteFixture(t)
	remoteOK(t, a, map[string]any{"action": "start", "ids": []string{"a", "b"}})
	first := awaitSession(t, sessions)
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.cache.mu.Lock()
		ready := len(a.cache.entries) == 2 && len(a.cache.jobs) == 0
		a.cache.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("next track not prefetched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.cache.ffmpeg = filepath.Join(t.TempDir(), "missing")
	close(first.finish)
	next := awaitSession(t, sessions)
	if next.metadata.ID != "b" {
		t.Fatal("unexpected next track")
	}
}

func TestWAVPCMReadsRF64(t *testing.T) {
	pcm := []byte("12345678")
	standard := testPCMBytes(pcm)
	var b bytes.Buffer
	b.WriteString("RF64")
	binary.Write(&b, binary.LittleEndian, uint32(0xffffffff))
	b.WriteString("WAVEds64")
	binary.Write(&b, binary.LittleEndian, uint32(28))
	binary.Write(&b, binary.LittleEndian, uint64(len(standard)+36-8))
	binary.Write(&b, binary.LittleEndian, uint64(len(pcm)))
	binary.Write(&b, binary.LittleEndian, uint64(len(pcm)/4))
	binary.Write(&b, binary.LittleEndian, uint32(0))
	b.Write(standard[12:])
	data := b.Bytes()
	binary.LittleEndian.PutUint32(data[76:], 0xffffffff)
	f, err := os.CreateTemp(t.TempDir(), "rf64")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(data)
	f.Seek(0, 0)
	reader, err := wavPCM(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, pcm) {
		t.Fatalf("RF64 data: %q %v", got, err)
	}
}
