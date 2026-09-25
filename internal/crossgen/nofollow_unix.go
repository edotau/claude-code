//go:build unix

package crossgen

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only; the kernel refuses a symlink at the final component (ELOOP).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
