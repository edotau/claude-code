//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package paths

import (
	"errors"
	"os"
	"syscall"
)

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// tryLock takes the exclusive lock without blocking; false with a nil error means another holder has it.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
