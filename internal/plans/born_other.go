//go:build !darwin && !freebsd && !netbsd

package plans

import (
	"io/fs"
	"time"
)

// fileBirth falls back to the mtime, never earlier than the true birth, so it can only skip a tick.
func fileBirth(fi fs.FileInfo) time.Time { return fi.ModTime() }
