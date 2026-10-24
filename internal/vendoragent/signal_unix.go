//go:build unix

package vendoragent

import (
	"errors"
	"syscall"
)

// interrupt sends SIGINT to pid's whole process group, then to pid; a target already reaped (ESRCH) is success.
func interrupt(pid int) error {
	var gerr error = syscall.ESRCH
	if pid > 1 {
		gerr = syscall.Kill(-pid, syscall.SIGINT)
	}
	err := syscall.Kill(pid, syscall.SIGINT)
	if err == nil || gerr == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// kill sends SIGKILL to pid's whole process group (the job leads its own session), then to pid; a target
// already reaped (ESRCH) is success — the caller verifies termination by polling.
func kill(pid int) error {
	var gerr error = syscall.ESRCH
	if pid > 1 {
		gerr = syscall.Kill(-pid, syscall.SIGKILL)
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == nil || gerr == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
