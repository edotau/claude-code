//go:build !windows

package clean

import (
	"errors"
	"syscall"
)

// pidAlive probes a pid with signal 0; EPERM still means the process exists under another owner.
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
