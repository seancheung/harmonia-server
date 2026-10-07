//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package airplay

import (
	"golang.org/x/sys/unix"
	"net"
)

func enableMulticastLoopback(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err = raw.Control(func(fd uintptr) { socketErr = unix.SetsockoptByte(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1) }); err != nil {
		return err
	}
	return socketErr
}
