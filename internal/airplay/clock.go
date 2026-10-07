package airplay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type clockServer struct {
	event, general *net.UDPConn
	peer           net.IP
	id             uint64
	ptp            bool
	fallbackErr    error
	done           chan struct{}
	wg             sync.WaitGroup
	lastProbe      atomic.Int64
}

func startClock(local, peer net.IP, id uint64, ptp bool) (*clockServer, error) {
	return startClockWithListener(local, peer, id, ptp, net.ListenUDP)
}

func startClockWithListener(local, peer net.IP, id uint64, ptp bool, listen func(string, *net.UDPAddr) (*net.UDPConn, error)) (*clockServer, error) {
	c, err := bindClock(local, peer, id, ptp, listen)
	// NTP uses a negotiated, unprivileged port within the same AirPlay 2
	// session. Do not hide unrelated failures such as an occupied PTP port.
	if ptp && errors.Is(err, os.ErrPermission) {
		c, fallbackErr := bindClock(local, peer, id, false, listen)
		if fallbackErr != nil {
			return nil, errors.Join(err, fallbackErr)
		}
		c.fallbackErr = err
		return c, nil
	}
	return c, err
}

func bindClock(local, peer net.IP, id uint64, ptp bool, listen func(string, *net.UDPAddr) (*net.UDPConn, error)) (*clockServer, error) {
	c := &clockServer{peer: peer, id: id, ptp: ptp, done: make(chan struct{})}
	port := 0
	if ptp {
		port = 319
	}
	var e error
	c.event, e = listen("udp4", &net.UDPAddr{IP: local, Port: port})
	if e != nil {
		return nil, fmt.Errorf("AirPlay clock port %d: %w", port, e)
	}
	if ptp {
		c.general, e = listen("udp4", &net.UDPAddr{IP: local, Port: 320})
		if e != nil {
			c.event.Close()
			return nil, fmt.Errorf("AirPlay clock port 320: %w", e)
		}
	}
	c.wg.Add(1)
	go c.receive(c.event)
	if ptp {
		c.wg.Add(2)
		go c.receive(c.general)
		go c.announce()
	}
	return c, nil
}
func (c *clockServer) close() {
	close(c.done)
	c.event.Close()
	if c.general != nil {
		c.general.Close()
	}
	c.wg.Wait()
}
func (c *clockServer) header(kind byte, length int, seq uint16) []byte {
	b := make([]byte, length)
	b[0] = 0x10 | kind
	b[1] = 2
	binary.BigEndian.PutUint16(b[2:], uint16(length))
	b[6] = 4 // unicast flag
	binary.BigEndian.PutUint64(b[20:], c.id)
	binary.BigEndian.PutUint16(b[28:], 1)
	binary.BigEndian.PutUint16(b[30:], seq)
	b[32] = 5
	b[33] = 0x7f
	return b
}
func ptpTime(b []byte, t time.Time) {
	sec := uint64(t.Unix())
	for i := 5; i >= 0; i-- {
		b[i] = byte(sec)
		sec >>= 8
	}
	binary.BigEndian.PutUint32(b[6:], uint32(t.Nanosecond()))
}
func (c *clockServer) receive(socket *net.UDPConn) {
	defer c.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, e := socket.ReadFromUDP(buf)
		if e != nil {
			return
		}
		if !addr.IP.Equal(c.peer) {
			continue
		}
		now := time.Now()
		b := buf[:n]
		if !c.ptp {
			if n != 32 || b[1]&0x7f != 0x52 {
				continue
			}
			out := make([]byte, 32)
			out[0] = 0x80
			out[1] = 0xd3
			copy(out[2:4], b[2:4])
			copy(out[8:16], b[24:32])
			binary.BigEndian.PutUint64(out[16:], ntp(now))
			binary.BigEndian.PutUint64(out[24:], ntp(time.Now()))
			_, _ = socket.WriteToUDP(out, addr)
			c.lastProbe.Store(now.UnixNano())
			continue
		}
		if n < 34 || b[1]&15 != 2 || int(binary.BigEndian.Uint16(b[2:])) > n {
			continue
		}
		seq := binary.BigEndian.Uint16(b[30:])
		kind := b[0] & 15
		switch kind {
		case 1, 2:
			c.lastProbe.Store(now.UnixNano())
			reply := byte(9)
			target := c.general
			dest := &net.UDPAddr{IP: c.peer, Port: 320}
			if kind == 2 {
				reply = 3
				target = c.event
				dest.Port = 319
			}
			out := c.header(reply, 54, seq)
			if kind == 2 {
				out[6] |= 2
			}
			ptpTime(out[34:], now)
			copy(out[44:], b[20:30])
			_, _ = target.WriteToUDP(out, dest)
			if kind == 2 {
				out = c.header(10, 54, seq)
				ptpTime(out[34:], time.Now())
				copy(out[44:], b[20:30])
				_, _ = c.general.WriteToUDP(out, &net.UDPAddr{IP: c.peer, Port: 320})
			}
		case 12:
			if n < 44 {
				continue
			}
			for p := 44; p+4 <= n; {
				typ := binary.BigEndian.Uint16(b[p:])
				l := int(binary.BigEndian.Uint16(b[p+2:]))
				if p+4+l > n {
					break
				}
				if typ == 4 && l >= 6 {
					out := c.header(12, 56, seq)
					copy(out[34:44], b[20:30])
					binary.BigEndian.PutUint16(out[44:], 5)
					binary.BigEndian.PutUint16(out[46:], 8)
					copy(out[48:54], b[p+4:p+10])
					out[55] = 1
					_, _ = c.general.WriteToUDP(out, &net.UDPAddr{IP: c.peer, Port: 320})
				}
				p += 4 + l
			}
		}
	}
}
func (c *clockServer) announce() {
	defer c.wg.Done()
	ticker := time.NewTicker(125 * time.Millisecond)
	defer ticker.Stop()
	var seq uint16
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			seq++
			if seq%8 == 1 {
				b := c.header(11, 64, seq)
				b[33] = 0
				b[47] = 128
				b[48] = 6
				b[49] = 0x21
				binary.BigEndian.PutUint16(b[50:], 0x436a)
				b[52] = 128
				binary.BigEndian.PutUint64(b[53:], c.id)
				b[63] = 0x20
				_, _ = c.general.WriteToUDP(b, &net.UDPAddr{IP: c.peer, Port: 320})
			}
			b := c.header(0, 44, seq)
			b[6] |= 2
			b[32] = 0
			b[33] = 0xfd
			now := time.Now()
			_, _ = c.event.WriteToUDP(b, &net.UDPAddr{IP: c.peer, Port: 319})
			b = c.header(8, 76, seq)
			b[32] = 2
			b[33] = 0xfd
			ptpTime(b[34:], now)
			binary.BigEndian.PutUint16(b[44:], 3)
			binary.BigEndian.PutUint16(b[46:], 28)
			copy(b[48:54], []byte{0, 0x80, 0xc2, 0, 0, 1})
			_, _ = c.general.WriteToUDP(b, &net.UDPAddr{IP: c.peer, Port: 320})
		}
	}
}
