//go:build linux

package proc

import (
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID is the uid of the process on the other end of conn (SO_PEERCRED).
func PeerUID(conn *net.UnixConn) (uint32, error) {
	return peerUID(conn, func(fd int) (uint32, error) {
		cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return 0, err
		}
		return cred.Uid, nil
	})
}
