//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package airplay

import (
	"errors"
	"net"
)

func enableMulticastLoopback(*net.UDPConn) error {
	return errors.New("multicast loopback is unsupported on this platform")
}
