//go:build unix

package paths

import (
	"os"
	"syscall"
)

// OwnerUID is the file's owning uid; false when the platform reports none.
func OwnerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
