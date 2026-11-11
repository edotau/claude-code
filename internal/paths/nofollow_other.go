//go:build !unix

package paths

import (
	"errors"
	"os"
)

// OpenNoFollow has no O_NOFOLLOW off unix: an Lstat check before opening keeps the contract (TOCTOU-racy).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing to follow symlink: " + path)
	}
	return os.OpenFile(path, flag, perm)
}
