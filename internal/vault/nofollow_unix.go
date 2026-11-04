//go:build unix

package vault

import (
	"os"
	"syscall"
)

// openAppendNoFollow opens path for 0600 append; the kernel refuses a symlink at the final component (ELOOP).
func openAppendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
}
