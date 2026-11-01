//go:build !unix

package secrets

import (
	"errors"
	"os"
)

// openAppendNoFollow has no O_NOFOLLOW off unix: an Lstat check keeps the no-symlink contract (TOCTOU-racy).
func openAppendNoFollow(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing to follow symlink: " + path)
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}
