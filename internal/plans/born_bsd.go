//go:build darwin || freebsd || netbsd

package plans

import (
	"io/fs"
	"syscall"
	"time"
)

func fileBirth(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Birthtimespec.Sec > 0 {
		return time.Unix(st.Birthtimespec.Unix())
	}
	return fi.ModTime()
}
