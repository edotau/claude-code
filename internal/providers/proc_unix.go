//go:build unix

package providers

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the child in its own process group so a deadline kills its whole pipeline, not just the shell.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// reapGroup kills whatever is left of cmd's process group once the command has returned.
func reapGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
