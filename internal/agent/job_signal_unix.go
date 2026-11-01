//go:build unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

// sysKill is the signal syscall; tests swap it to observe targets without signalling.
var sysKill = syscall.Kill

// interrupt sends SIGINT to pid's whole process group, then to pid; a target already reaped (ESRCH) is success.
func interrupt(pid int) error {
	var gerr error = syscall.ESRCH
	if pid > 1 {
		gerr = sysKill(-pid, syscall.SIGINT)
	}
	err := sysKill(pid, syscall.SIGINT)
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
		gerr = sysKill(-pid, syscall.SIGKILL)
	}
	err := sysKill(pid, syscall.SIGKILL)
	if err == nil || gerr == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// ownGroup puts the child in its own process group so a deadline kills its grandchildren too.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
