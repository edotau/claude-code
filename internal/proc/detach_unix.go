//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// Detach starts cmd in its own session so it outlives the parent's terminal.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// OwnGroup runs cmd in its own process group and kills the whole group on cancel.
func OwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// execImage replaces the current process image so the child owns the TTY and signals; returns only on failure.
var execImage = func(bin string, argv, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv...), env)
}
