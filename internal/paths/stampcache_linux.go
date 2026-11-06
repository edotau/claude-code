//go:build linux

package paths

import (
	"io/fs"
	"syscall"
)

func statIdentity(fi fs.FileInfo) (uint64, int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), st.Ctim.Nano()
	}
	return 0, 0
}
