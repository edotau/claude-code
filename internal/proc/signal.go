package proc

import (
	"errors"
	"syscall"
)

// SignalGroup sends sig to pid's whole process group, then to pid; a target already reaped (ESRCH) is success.
func SignalGroup(pid int, sig syscall.Signal) error {
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
