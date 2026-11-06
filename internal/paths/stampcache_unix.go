//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package paths

import (
	"io/fs"
	"syscall"
)

func statIdentity(fi fs.FileInfo) (uint64, int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), st.Ctimespec.Nano()
	}
	return 0, 0
}
