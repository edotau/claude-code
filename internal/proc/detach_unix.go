//go:build unix

package proc

import (
	"syscall"
)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Exec replaces the current process image so the child owns the TTY and signals; returns only on failure.
func Exec(bin string, argv, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv...), env)
}
