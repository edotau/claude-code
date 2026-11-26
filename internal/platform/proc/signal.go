package proc

import (
	"errors"
	"syscall"
)

// SignalGroup signals pid's group, then pid; ESRCH (already reaped) is success and pid <= 0 is EINVAL.
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL // 0 and negatives address our own group, another group or every process
	}
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
