//go:build unix

package worktree

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel runs cmd in its own process group and kills the whole group on context cancel.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
