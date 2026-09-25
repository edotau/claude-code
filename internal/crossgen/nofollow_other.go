//go:build !unix

package crossgen

import (
	"errors"
	"os"
)

// openNoFollow has no O_NOFOLLOW off unix: an Lstat check before opening keeps the no-symlink contract (TOCTOU-racy).
func openNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing to follow symlink: " + path)
	}
	return os.Open(path)
}
