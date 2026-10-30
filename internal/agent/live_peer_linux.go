//go:build linux

package agent

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process on the other end of conn (SO_PEERCRED).
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		if cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); gerr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, gerr
}
