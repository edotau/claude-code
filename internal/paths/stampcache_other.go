//go:build !darwin && !freebsd && !netbsd && !linux

package paths

import "io/fs"

func statIdentity(fi fs.FileInfo) (uint64, int64) {
	return 0, 0
}
