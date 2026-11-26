//go:build unix

package hosttest

import (
	"os/exec"
	"syscall"
)

// ownGroup kills cmd's whole process group on cancel; a hook child would otherwise outlive the killed git.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
