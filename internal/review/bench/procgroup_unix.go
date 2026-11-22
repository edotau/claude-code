//go:build !windows

package bench

import (
	"os/exec"
	"syscall"
)

// ownGroup makes cmd lead its own process group and kills the whole group on cancel.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
