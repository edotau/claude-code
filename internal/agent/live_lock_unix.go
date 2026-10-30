//go:build unix

package agent

import (
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking exclusive flock on f; the kernel drops it when the holder dies.
func tryLockFile(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
