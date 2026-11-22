//go:build !darwin && !linux

package serve

import (
	"errors"
	"net"
)

// peerPID has no portable form here, so the daemon refuses every admit and each job runs directly.
func peerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("peer pid unsupported on this platform")
}
