//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// Detach starts cmd in its own session so it outlives the parent's terminal.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Exec replaces the current process image so the child owns the TTY and signals; returns only on failure.
func Exec(bin string, argv, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv...), env)
}
