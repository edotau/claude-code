//go:build unix

package proc

import "syscall"

// Alive reports whether pid is a live process (signal-0 probe). EPERM means alive but not ours.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
