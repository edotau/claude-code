//go:build darwin

package serve

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID is the pid of the process on the other end of c (LOCAL_PEERPID).
func peerPID(c *net.UnixConn) (int, error) {
	return peerCred(c, func(fd int) (int, error) { return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID) })
}
