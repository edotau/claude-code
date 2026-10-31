//go:build !windows

package agent

import (
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
