package airplay

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Session struct {
	legacy                bool
	aesKey, aesIV         []byte
	active                atomic.Bool
	ctrl                  *control
	event                 net.Conn
	data, rtcp            *net.UDPConn
	target, controlTarget *net.UDPAddr
	clock                 *clockServer
	secret                []byte
	seq                   uint16
	stamp, ssrc           uint32
	counter               uint64
	lead                  time.Duration
	done                  chan struct{}
	once                  sync.Once
	wg                    sync.WaitGroup
	mu                    sync.Mutex
	packets               map[uint16][]byte
	failure               error
}

func dial(device Device, id string) (*control, error) {
	if device.Port < 1 || device.Port > 65535 || net.ParseIP(device.Address) == nil {
		return nil, errors.New("invalid AirPlay endpoint")
	}
	conn, e := net.DialTimeout("tcp4", net.JoinHostPort(device.Address, strconv.Itoa(device.Port)), 5*time.Second)
	if e != nil {
		return nil, e
	}
	c := &control{conn: conn, rw: conn, reader: bufio.NewReader(conn), id: strings.ToUpper(id), url: "rtsp://" + net.JoinHostPort(device.Address, strconv.Itoa(device.Port)) + "/" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	c.legacy = device.Type == "AirPlay 1"
	if c.legacy {
		c.url = "rtsp://" + conn.LocalAddr().(*net.TCPAddr).IP.String() + "/" + strconv.FormatInt(time.Now().UnixNano(), 10)
		return c, nil
	}
	if _, e = c.request("GET", "/info", "", nil, nil); e != nil {
		conn.Close()
		return nil, e
	}
	return c, nil
}
func Connect(ctx context.Context, device Device, id string, cred *Credentials) (s *Session, err error) {
	if device.Type == "AirPlay 1" {
		return connectRAOP(ctx, device, id, true)
	}
	if cred != nil {
		id = cred.ID
	}
	c, e := dial(device, id)
	if e != nil {
		return nil, e
	}
	s = &Session{ctrl: c, done: make(chan struct{}), packets: map[uint16][]byte{}, lead: time.Second}
	ok := false
	defer func(session *Session) {
		if !ok {
			session.Close()
		}
	}(s)
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	secret, e := authenticate(c, cred)
	if e != nil {
		return nil, e
	}
	s.secret = secret
	c.encrypt(secret)
	local := c.conn.LocalAddr().(*net.TCPAddr).IP
	peer := net.ParseIP(device.Address)
	clockID, _ := strconv.ParseUint(id, 16, 64)
	s.clock, e = startClock(local, peer, clockID, device.Features&(1<<41) != 0)
	if e != nil {
		return nil, e
	}
	for _, dst := range []**net.UDPConn{&s.data, &s.rtcp} {
		*dst, e = net.ListenUDP("udp4", &net.UDPAddr{IP: local})
		if e != nil {
			return nil, e
		}
	}
	uuid := fmt.Sprintf("%s-%s-%s-%s-%s", Identity()[:8], Identity()[:4], Identity()[:4], Identity()[:4], Identity()[:12])
	mac := []string{}
	for i := 0; i+2 <= len(id); i += 2 {
		mac = append(mac, id[i:i+2])
	}
	setup := map[string]any{"deviceID": strings.Join(mac, ":"), "sessionUUID": uuid, "name": "Harmonia", "timingProtocol": "NTP", "timingPort": s.clock.event.LocalAddr().(*net.UDPAddr).Port}
	if s.clock.ptp {
		delete(setup, "timingPort")
		setup["timingProtocol"] = "PTP"
		setup["macAddress"] = strings.Join(mac, ":")
		setup["groupUUID"] = uuid
		setup["groupContainsGroupLeader"] = false
		peerInfo := map[string]any{"ID": uuid, "ClockID": clockID, "DeviceType": 0, "SupportsClockPortMatchingOverride": false, "Addresses": []any{local.String()}}
		setup["timingPeerInfo"] = peerInfo
		setup["timingPeerList"] = []any{peerInfo}
	}
	response, e := c.property("SETUP", "", setup)
	if e != nil {
		if s.clock.fallbackErr != nil {
			return nil, fmt.Errorf("AirPlay 2 NTP setup failed after PTP ports were denied (%v): %w", s.clock.fallbackErr, e)
		}
		return nil, e
	}
	if port := number(response["eventPort"]); port > 0 && port <= 65535 {
		s.event, e = net.DialTimeout("tcp4", net.JoinHostPort(device.Address, strconv.Itoa(int(port))), 5*time.Second)
		if e != nil {
			return nil, e
		}
		s.wg.Add(1)
		go s.events()
	}
	if _, e = c.request("RECORD", "", "", nil, nil); e != nil {
		return nil, e
	}
	if s.clock.ptp {
		if _, e = c.request("SETPEERS", "", "application/x-apple-binary-plist", plist([]any{device.Address, local.String()}), nil); e != nil {
			return nil, e
		}
	}
	s.ssrc = uint32(time.Now().UnixNano())
	s.stamp = s.ssrc
	s.seq = uint16(s.ssrc)
	stream := map[string]any{"type": 96, "audioFormat": 0x40000, "ct": 2, "sr": SampleRate, "spf": Frames, "shk": secret[:32], "controlPort": s.rtcp.LocalAddr().(*net.UDPAddr).Port, "dataPort": s.data.LocalAddr().(*net.UDPAddr).Port, "latencyMin": 11025, "latencyMax": 88200, "audioMode": "default", "isMedia": true, "supportsDynamicStreamID": false, "streamConnectionID": uint64(s.ssrc)}
	response, e = c.property("SETUP", "", map[string]any{"streams": []any{stream}})
	if e != nil {
		return nil, e
	}
	streams, valid := response["streams"].([]any)
	if !valid || len(streams) != 1 {
		return nil, errors.New("receiver did not negotiate a single audio stream")
	}
	v, valid := streams[0].(map[string]any)
	if !valid {
		return nil, errors.New("invalid stream response")
	}
	dp, cp := number(v["dataPort"]), number(v["controlPort"])
	if dp == 0 || dp > 65535 || cp == 0 || cp > 65535 {
		return nil, errors.New("invalid receiver audio ports")
	}
	s.target = &net.UDPAddr{IP: peer, Port: int(dp)}
	s.controlTarget = &net.UDPAddr{IP: peer, Port: int(cp)}
	if low := number(v["latencyMin"]); low > 0 {
		s.lead = max(s.lead, time.Duration(low)*time.Second/SampleRate)
	}
	if s.lead > 2*time.Second {
		return nil, errors.New("receiver latency exceeds realtime sender window")
	}
	if s.clock.ptp {
		s.ssrc = 0
	}
	s.wg.Add(2)
	go s.retransmits()
	go s.feedback()
	s.active.Store(true)
	ok = true
	return s, nil
}
func number(v any) uint64 { n, _ := v.(uint64); return n }
func (s *Session) fail(e error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = e
	}
	s.mu.Unlock()
}
func (s *Session) err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.failure }
func (s *Session) events() {
	defer s.wg.Done()
	wire := newSecure(s.event, s.secret, true)
	reader := bufio.NewReader(wire)
	for {
		m, e := readMessage(reader)
		if e != nil {
			select {
			case <-s.done:
				return
			default:
				s.fail(fmt.Errorf("AirPlay event channel: %w", e))
				return
			}
		}
		reply := "RTSP/1.0 200 OK\r\nServer: Harmonia\r\n"
		if seq := m.header.Get("CSeq"); seq != "" {
			reply += "CSeq: " + seq + "\r\n"
		}
		reply += "\r\n"
		_ = s.event.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if e = writeAll(wire, []byte(reply)); e != nil {
			s.fail(e)
			return
		}
	}
}
func (s *Session) feedback() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			method, path := "POST", "/feedback"
			if s.legacy {
				method, path = "OPTIONS", "*"
			}
			if _, e := s.ctrl.request(method, path, "", nil, nil); e != nil {
				s.fail(e)
				return
			}
		}
	}
}
func (s *Session) retransmits() {
	defer s.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, e := s.rtcp.ReadFromUDP(buf)
		if e != nil {
			return
		}
		if n < 8 || !addr.IP.Equal(s.target.IP) || buf[1]&0x7f != 0x55 {
			continue
		}
		start := binary.BigEndian.Uint16(buf[4:])
		count := min(int(binary.BigEndian.Uint16(buf[6:])), 512)
		for i := 0; i < count; i++ {
			s.mu.Lock()
			b := s.packets[start+uint16(i)]
			s.mu.Unlock()
			if b != nil {
				out := append([]byte{0x80, 0xd6, buf[2], buf[3]}, b...)
				_, _ = s.rtcp.WriteToUDP(out, addr)
			}
		}
	}
}
func (s *Session) Volume(volume int) error {
	db := -144.0
	if volume > 0 {
		db = -30 + float64(min(volume, 100))*0.3
	}
	_, e := s.ctrl.request("SET_PARAMETER", "", "text/parameters", []byte(fmt.Sprintf("volume: %.6f\r\n", db)), nil)
	return e
}
func (s *Session) syncAudio(stamp uint32, at time.Time, first bool) error {
	size := 20
	if s.clock.ptp {
		size = 28
	}
	b := make([]byte, size)
	b[0] = 0x80
	if first {
		b[0] = 0x90
	}
	b[1] = 0xd4
	binary.BigEndian.PutUint16(b[2:], 7) // AirPlay sync flags, not an RTCP length
	latency := uint32(s.lead * SampleRate / time.Second)
	if s.clock.ptp {
		binary.BigEndian.PutUint32(b[4:], stamp)
		binary.BigEndian.PutUint32(b[16:], stamp+latency)
		b[1] = 0xd7
		binary.BigEndian.PutUint64(b[8:], uint64(at.UnixNano()))
		binary.BigEndian.PutUint64(b[20:], s.clock.id)
	} else {
		// Match the audible position to its clock instant, and advertise the
		// current transmission head (ahead by the receiver's buffer lead).
		binary.BigEndian.PutUint32(b[4:], stamp-latency)
		binary.BigEndian.PutUint32(b[16:], stamp)
		binary.BigEndian.PutUint64(b[8:], ntp(at.Add(-s.lead)))
	}
	_, e := s.rtcp.WriteToUDP(b, s.controlTarget)
	return e
}

func (s *Session) waitForClock(ctx context.Context) error {
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var firstProbe time.Time
	for {
		if err := s.err(); err != nil {
			return err
		}
		last := s.clock.lastProbe.Load()
		if last != 0 && (!s.clock.ptp || time.Since(time.Unix(0, last)) < time.Second) {
			if firstProbe.IsZero() {
				firstProbe = time.Now()
			}
			if !s.clock.ptp || time.Since(firstProbe) >= 2500*time.Millisecond {
				return nil
			}
		} else {
			firstProbe = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if s.clock.ptp {
				return errors.New("receiver did not synchronize with AirPlay PTP clock (UDP 319/320)")
			}
			return errors.New("receiver accepted AirPlay NTP setup but sent no timing requests; audio playback has not started")
		case <-tick.C:
		}
	}
}

// Stream reports the scheduled position, not proof of audible receiver output.
func (s *Session) Stream(ctx context.Context, pcm io.Reader, progress func(time.Duration)) error {
	if err := s.waitForClock(ctx); err != nil {
		return err
	}
	var start time.Time
	var samples int64
	lastSync := time.Time{}
	buf := make([]byte, PCMBytes)
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := s.err(); e != nil {
			return e
		}
		if s.clock.ptp && time.Since(time.Unix(0, s.clock.lastProbe.Load())) > 5*time.Second {
			return errors.New("AirPlay PTP timing exchanges stopped")
		}
		n, e := io.ReadFull(pcm, buf)
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			return e
		}
		if n == 0 {
			break
		}
		if n%4 != 0 {
			return errors.New("unaligned PCM input")
		}
		clear(buf[n:])
		if start.IsZero() {
			start = time.Now().Add(s.lead)
		}
		audible := start.Add(time.Duration(samples) * time.Second / SampleRate)
		sendAt := audible.Add(-s.lead)
		if e := waitUntil(ctx, sendAt); e != nil {
			return e
		}
		if time.Since(sendAt) > 300*time.Millisecond {
			return errors.New("audio decoder underrun; retry playback")
		}
		if time.Since(lastSync) >= time.Second {
			if e = s.syncAudio(s.stamp, audible, lastSync.IsZero()); e != nil {
				return e
			}
			lastSync = time.Now()
		}
		var packet []byte
		if s.legacy {
			packet, e = raopPacket(s.aesKey, s.aesIV, s.seq, s.stamp, s.ssrc, buf)
		} else {
			packet, e = audioPacket(s.secret[:32], s.seq, s.stamp, s.ssrc, s.counter, buf)
		}
		if e != nil {
			return e
		}
		if s.counter == 0 {
			packet[1] |= 0x80
		}
		s.mu.Lock()
		s.packets[s.seq] = packet
		delete(s.packets, s.seq-512)
		s.mu.Unlock()
		if _, e = s.data.WriteToUDP(packet, s.target); e != nil {
			return e
		}
		s.seq++
		s.stamp += Frames
		s.counter++
		samples += int64(n / 4)
		elapsed := time.Since(start)
		if elapsed > 0 {
			progress(min(elapsed, time.Duration(samples)*time.Second/SampleRate))
		}
		if n < PCMBytes {
			break
		}
	}
	if start.IsZero() {
		return errors.New("decoder produced no audio")
	}
	end := start.Add(time.Duration(samples) * time.Second / SampleRate)
	for time.Now().Before(end) {
		if e := s.err(); e != nil {
			return e
		}
		if e := waitUntil(ctx, minTime(end, time.Now().Add(50*time.Millisecond))); e != nil {
			return e
		}
		progress(min(max(time.Duration(0), time.Since(start)), time.Duration(samples)*time.Second/SampleRate))
	}
	return nil
}
func waitUntil(ctx context.Context, at time.Time) error {
	delay := time.Until(at)
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.done)
		if s.ctrl != nil {
			if s.active.Load() && s.ctrl.mu.TryLock() {
				_, _ = s.ctrl.exchange("TEARDOWN", "", "", nil, nil, 500*time.Millisecond)
				s.ctrl.mu.Unlock()
			}
			s.ctrl.conn.Close()
		}
		if s.event != nil {
			s.event.Close()
		}
		if s.data != nil {
			s.data.Close()
		}
		if s.rtcp != nil {
			s.rtcp.Close()
		}
		if s.clock != nil {
			s.clock.close()
		}
		s.wg.Wait()
	})
}
func (s *Session) Stop() { _, _ = s.ctrl.request("TEARDOWN", "", "", nil, nil); s.Close() }
