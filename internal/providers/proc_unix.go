//go:build unix

package providers

import (
	"os/exec"
	"syscall"
)

// outputInGroup runs cmd in a group led by a sleeping sentinel, then kills the group before reaping the sentinel,
// so nothing cmd started outlives it and the group id cannot have been reused by the time it is signalled.
func outputInGroup(cmd *exec.Cmd) ([]byte, error) {
	lead := exec.Command("sleep", "86400")
	lead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := lead.Start(); err != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		return cmd.Output()
	}
	pg := lead.Process.Pid
	defer func() { _ = syscall.Kill(-pg, syscall.SIGKILL); _ = lead.Wait() }()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pg}
	cmd.Cancel = func() error { return syscall.Kill(-pg, syscall.SIGKILL) }
	return cmd.Output()
}
