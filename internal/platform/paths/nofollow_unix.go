//go:build unix

package paths

import (
	"os"
	"syscall"
)

// OpenNoFollow opens path with flag and perm; the kernel refuses a symlink at the final component (ELOOP).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}
