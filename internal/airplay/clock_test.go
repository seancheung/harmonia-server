package airplay

import (
	"errors"
	"net"
	"os"
	"reflect"
	"strconv"
	"syscall"
	"testing"
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
