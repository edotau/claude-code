//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the child in its own process group so a deadline kills its grandchildren too.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
