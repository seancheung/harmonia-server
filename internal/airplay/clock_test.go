package airplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClockPermissionFallback(t *testing.T) {
	for _, deniedPort := range []int{319, 320} {
		t.Run(strconv.Itoa(deniedPort), func(t *testing.T) {
			var ports []int
			var first *net.UDPConn
			listen := func(network string, addr *net.UDPAddr) (*net.UDPConn, error) {
				ports = append(ports, addr.Port)
				if addr.Port == deniedPort {
					return nil, &net.OpError{Op: "listen", Net: network, Err: os.ErrPermission}
				}
				conn, err := net.ListenUDP(network, &net.UDPAddr{IP: addr.IP})
				if addr.Port == 319 {
					first = conn
				}
				return conn, err
			}
			c, err := startClockWithListener(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 1, true, listen)
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			if c.ptp || c.general != nil || !errors.Is(c.fallbackErr, os.ErrPermission) {
				t.Fatal("expected NTP fallback")
			}
			want := []int{319, 0}
			if deniedPort == 320 {
				want = []int{319, 320, 0}
			}
			if !reflect.DeepEqual(ports, want) {
				t.Fatalf("ports %v, want %v", ports, want)
			}
			if first != nil {
				if _, err := first.WriteToUDP([]byte{0}, first.LocalAddr().(*net.UDPAddr)); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("PTP socket leaked: %v", err)
				}
			}
		})
	}
}

func TestClockReadFailureReachesStream(t *testing.T) {
	c, err := startClock(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	// Simulate a socket failure without the normal clock shutdown signal.
	c.event.Close()
	c.wg.Wait()
	s := &Session{clock: c}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = s.Stream(ctx, bytes.NewReader(nil), func(time.Duration) { t.Error("advanced after timing failure") })
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("lost timing read error: %v", err)
	}
}

func TestClockWriteFailureAndNormalClose(t *testing.T) {
	for _, closing := range []bool{false, true} {
		c, err := startClock(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if closing {
			c.close()
		}
		// An invalid destination fails deterministically without relying on ICMP.
		if c.send(c.event, []byte{0}, &net.UDPAddr{IP: c.peer, Port: -1}) {
			t.Fatal("write unexpectedly succeeded")
		}
		if (c.err() != nil) == closing {
			t.Fatalf("closing=%v error=%v", closing, c.err())
		}
		if !closing {
			c.close()
		}
	}
}

func TestTimingDiagnosticsStayInServerLogs(t *testing.T) {
	c, err := startClock(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 123, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	s := &Session{clock: c}
	message := "AirPlay PTP timing exchanges stopped"
	err = s.clockTimeout(message)
	if err.Error() != message {
		t.Fatalf("client received diagnostics: %v", err)
	}
	for _, fragment := range []string{"local=", "receiver=127.0.0.1", "clock=000000000000007b", "no successful timing replies"} {
		if !strings.Contains(Diagnostic(err), fragment) {
			t.Fatalf("log missing %q: %s", fragment, Diagnostic(err))
		}
	}
	c.recordError(&net.OpError{Op: "read", Net: "udp", Err: net.ErrClosed})
	err = fmt.Errorf("playback: %w", c.err())
	if strings.Contains(err.Error(), "closed") || !strings.Contains(Diagnostic(err), "closed") || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("incorrect diagnostic separation: client=%v log=%s", err, Diagnostic(err))
	}
}

func TestClockDoesNotHidePortConflict(t *testing.T) {
	calls := 0
	_, err := startClockWithListener(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 1, true, func(string, *net.UDPAddr) (*net.UDPConn, error) {
		calls++
		return nil, syscall.EADDRINUSE
	})
	if !errors.Is(err, syscall.EADDRINUSE) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}
