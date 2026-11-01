//go:build !linux && !darwin

package proc

import "net"

// PeerUID has no portable form here; the socket's 0600 mode in the 0700 dispatch dir is the only guard.
func PeerUID(*net.UnixConn) (uint32, error) { return 0, ErrPeerUnsupported }
