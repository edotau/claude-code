//go:build linux

package serve

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID is the pid of the process on the other end of c (SO_PEERCRED).
func peerPID(c *net.UnixConn) (int, error) {
	return peerCred(c, func(fd int) (int, error) {
		cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return 0, err
		}
		return int(cred.Pid), nil
	})
}
