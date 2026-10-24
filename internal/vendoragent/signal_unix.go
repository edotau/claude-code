//go:build unix

package vendoragent

import "syscall"

// interrupt sends SIGINT to pid.
func interrupt(pid int) error { return syscall.Kill(pid, syscall.SIGINT) }

// kill sends SIGKILL to pid.
func kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
