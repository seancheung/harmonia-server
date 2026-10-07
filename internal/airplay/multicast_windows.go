package airplay

import (
	"golang.org/x/sys/windows"
	"net"
)

func enableMulticastLoopback(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err = raw.Control(func(fd uintptr) {
		socketErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_MULTICAST_LOOP, 1)
	}); err != nil {
		return err
	}
	return socketErr
}
