//go:build !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !linux

package paths

import "io/fs"

func statIdentity(fi fs.FileInfo) (uint64, int64) {
	return 0, 0
}
