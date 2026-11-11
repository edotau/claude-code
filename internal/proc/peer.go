package proc

import (
	"errors"
	"net"
)

// ErrPeerUnsupported marks a platform with no socket peer-credential call.
var ErrPeerUnsupported = errors.New("peer credentials unsupported on this platform")

// peerUID runs credUID on conn's file descriptor and returns the uid it reads.
func peerUID(conn *net.UnixConn, credUID func(fd int) (uint32, error)) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var gerr error
	if err := raw.Control(func(fd uintptr) { uid, gerr = credUID(int(fd)) }); err != nil {
		return 0, err
	}
	return uid, gerr
}
