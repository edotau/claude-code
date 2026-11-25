//go:build darwin || linux

package server

import "syscall"

// reusePort lets a replacement bind the port while the old daemon still listens; both must set it.
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return serr
}
