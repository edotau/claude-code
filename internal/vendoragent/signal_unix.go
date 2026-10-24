//go:build unix

package vendoragent

import "syscall"

// interrupt sends SIGINT to pid.
func interrupt(pid int) error { return syscall.Kill(pid, syscall.SIGINT) }
