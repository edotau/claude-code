//go:build !linux && !darwin

package agent

import (
	"errors"
	"net"
)

// peerUID has no portable form here; the holder fails closed and refuses every connection.
func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peer credentials unsupported on this platform")
}
