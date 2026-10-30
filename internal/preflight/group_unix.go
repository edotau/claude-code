//go:build unix

package preflight

import (
	"os/exec"
	"syscall"
)

// groupKill runs cmd in its own process group and kills the whole group on cancel.
func groupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
