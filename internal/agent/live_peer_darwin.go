//go:build darwin

package agent

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process on the other end of conn (LOCAL_PEERCRED).
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Xucred
		if cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); gerr == nil {
			uid = cred.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, gerr
}
