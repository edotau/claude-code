package job

import (
	"errors"
	"syscall"
)

// signalGroup sends sig to pid's whole process group, then to pid; a target already reaped (ESRCH) is success.
func signalGroup(pid int, sig syscall.Signal) error {
	var gerr error = syscall.ESRCH
	if pid > 1 {
		gerr = sysKill(-pid, sig)
	}
	err := sysKill(pid, sig)
	if err == nil || gerr == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// interrupt sends SIGINT to pid's process group and pid.
func interrupt(pid int) error { return signalGroup(pid, syscall.SIGINT) }

// kill sends SIGKILL to pid's process group and pid; the caller verifies termination by polling.
func kill(pid int) error { return signalGroup(pid, syscall.SIGKILL) }
