package teasipper

import (
	"net"
	"syscall"
)

func tcpSocketOption(conn *Conn, level, option int) (int, error) {
	raw, err := conn.raw.(*net.TCPConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var value int
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		value, socketErr = syscall.GetsockoptInt(int(fd), level, option)
	}); err != nil {
		return 0, err
	}
	return value, socketErr
}
