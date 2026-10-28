//go:build !windows

package copilotreg

import (
	"errors"
	"os"
	"syscall"
)

// ownerUID is the file's owning uid.
func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// pidAlive probes a pid with signal 0; EPERM still means the process exists under another owner.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
