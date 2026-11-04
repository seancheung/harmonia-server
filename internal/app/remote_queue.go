package app

import (
	"context"
	"errors"
	"io"
	"log"
	"slices"
	"time"

	"github.com/harmonia/harmonia-server/internal/airplay"
)

// Track boundaries are expressed in samples, including boundaries inside an RTP
// packet. Stream sees one PCM reader and pads only the final packet of the queue.
type continuousSession interface {
	remoteSession
	MetadataAt(airplay.TrackMetadata, uint64) error
	ArtworkAt([]byte, uint64) error
}

type queuedTrack struct {
	track              Track
	saved              RemoteSaved
	pcm                io.Reader
	release            func()
	start, frames      int64
	counted, completed bool
}

type preparedTrack struct {
	cancel context.CancelFunc
	done   chan struct{}
	track  *queuedTrack
}

func cloneRemoteSaved(s RemoteSaved) RemoteSaved {
	s.Queue = slices.Clone(s.Queue)
	s.ShuffleVisited = slices.Clone(s.ShuffleVisited)
	return s
}

func pcmFrames(pcm io.Reader) int64 {
	if sized, ok := pcm.(interface{ Size() int64 }); ok {
		return sized.Size() / 4
	}
	return 0
}

func (r *Remote) prepareTrack(ctx context.Context, current RemoteSaved) *preparedTrack {
	ctx, cancel := context.WithCancel(ctx)
	p := &preparedTrack{cancel: cancel, done: make(chan struct{})}
	// Plan on an isolated snapshot; shuffle/history are committed only when audible.
	planner := &Remote{saved: cloneRemoteSaved(current)}
	if current.Waiting || current.Deadline > 0 || current.Finish || !planner.advanceLocked(false) {
		close(p.done)
		return p
	}
	planner.saved.Position = 0
	planner.resetHistoryLocked(0)
	go func() {
		defer close(p.done)
		saved := planner.saved
		track, path, err := trackFromState(r.app.store.Read(), saved.Queue[saved.Index])
		if err != nil {
			return
		}
		pcm, release, err := r.openPCM(ctx, track, path, saved)
		if err != nil {
			return
		}
		frames := pcmFrames(pcm)
		if frames <= 0 {
			release()
			return
		}
		p.track = &queuedTrack{track: track, saved: saved, pcm: pcm, release: release, frames: frames}
	}()
	return p
}

func (p *preparedTrack) close() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
	if p.track != nil && p.track.release != nil {
		p.track.release()
		p.track.release = nil
	}
}

type remoteQueueReader struct {
	r           *Remote
	ctx         context.Context
	session     continuousSession
	generation  uint64
	current     *queuedTrack
	pending     []*queuedTrack
	next        *preparedTrack
	eof         bool
	skipArtwork bool
}

func (q *remoteQueueReader) close() {
	q.next.close()
	if q.current.release != nil {
		q.current.release()
		q.current.release = nil
	}
}

func (q *remoteQueueReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for !q.eof {
		if err := q.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := q.current.pcm.Read(b)
		// Preserve bytes before looking ahead; io.ReadFull will join the partial
		// packet to the next song without inserting silence.
		if n > 0 {
			return n, errWithoutEOF(err)
		}
		if err != io.EOF {
			return n, err
		}
		if q.current.release != nil {
			q.current.release()
			q.current.release = nil
		}
		q.eof = true
		select {
		case <-q.next.done:
		default:
			// Never block the RTP clock on conversion. Drain this song and let
			// the normal completion path start the successor when it is ready.
			return 0, io.EOF
		}
		next := q.next.track
		if next == nil || len(q.pending) >= 128 {
			return 0, io.EOF
		}
		q.r.mu.Lock()
		live := q.r.saved
		valid := q.generation == q.r.generation && slices.Equal(live.Queue, q.current.saved.Queue) &&
			live.Repeat == q.current.saved.Repeat && live.Shuffle == q.current.saved.Shuffle &&
			!live.Waiting && live.Deadline == 0 && !live.Finish
		q.r.mu.Unlock()
		if !valid {
			return 0, io.EOF
		}
		next.start = q.current.start + q.current.frames
		var artwork []byte
		if !q.skipArtwork {
			artwork, _ = remoteArtwork(next.track.Cover)
		}
		m := airplay.TrackMetadata{ID: next.track.ID, Title: next.track.Title, Artist: next.track.Artist, Album: next.track.Album, AlbumArtist: next.track.AlbumArtist, Number: next.track.Number, Disc: next.track.Disc, Duration: time.Duration(next.track.Duration * float64(time.Second)), HasArtwork: len(artwork) > 0}
		if err := q.session.MetadataAt(m, uint64(next.start)); err != nil {
			return 0, err
		}
		if !q.skipArtwork {
			if err := q.session.ArtworkAt(artwork, uint64(next.start)); err != nil {
				log.Printf("AirPlay queued artwork for track %s: %v", next.track.ID, err)
				// A broken control channel must not cause replay of an earlier song.
				if errors.Is(err, airplay.ErrArtworkTransport) {
					return 0, err
				}
			}
		}
		q.next.track = nil // transfer ownership before closing the preparation job
		q.next.close()
		q.current = next
		q.pending = append(q.pending, next)
		q.next = q.r.prepareTrack(q.ctx, next.saved)
		q.eof = false
	}
	return 0, io.EOF
}

func errWithoutEOF(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

func (q *remoteQueueReader) progress(position time.Duration) {
	for len(q.pending) > 0 {
		t := q.pending[0]
		start := time.Duration(t.start) * time.Second / airplay.SampleRate
		end := time.Duration(t.start+t.frames) * time.Second / airplay.SampleRate
		elapsed := min(max(position-start, 0), end-start)
		complete := position >= end
		if !t.counted && !t.completed && (t.saved.Listened+elapsed.Seconds() >= 15 || complete) {
			t.counted = q.r.app.store.RecordPlayed(t.track.ID, t.saved.PlaybackSession, t.saved.Listened+elapsed.Seconds(), complete, t.saved.Seeked, time.Now()) == nil
		}
		q.r.mu.Lock()
		if q.generation == q.r.generation && slices.Equal(q.r.saved.Queue, t.saved.Queue) &&
			q.r.saved.Repeat == t.saved.Repeat && q.r.saved.Shuffle == t.saved.Shuffle {
			if q.r.saved.PlaybackSession != t.saved.PlaybackSession {
				q.r.saved.ShuffleVisited = slices.Clone(t.saved.ShuffleVisited)
			}
			q.r.state = "play"
			q.r.saved.Index = t.saved.Index
			q.r.saved.Position = t.saved.Position + int(elapsed.Milliseconds())
			q.r.saved.Listened = t.saved.Listened + elapsed.Seconds()
			q.r.saved.PlaybackSession = t.saved.PlaybackSession
			q.r.saved.Counted, q.r.saved.Seeked = t.counted, t.saved.Seeked
		}
		q.r.mu.Unlock()
		if !complete {
			return
		}
		t.completed = true
		if len(q.pending) == 1 {
			return
		}
		q.pending = q.pending[1:]
	}
}

func (r *Remote) streamQueue(ctx context.Context, session continuousSession, pcm io.Reader, release func(), track Track, saved RemoteSaved, generation uint64, skipArtwork bool) error {
	first := &queuedTrack{track: track, saved: cloneRemoteSaved(saved), pcm: pcm, release: release, frames: pcmFrames(pcm), counted: saved.Counted}
	q := &remoteQueueReader{r: r, ctx: ctx, session: session, generation: generation, current: first, pending: []*queuedTrack{first}, skipArtwork: skipArtwork}
	q.next = r.prepareTrack(ctx, first.saved)
	defer q.close()
	return session.Stream(ctx, q, q.progress)
}
