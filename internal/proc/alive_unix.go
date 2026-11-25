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

// GroupAlive reports whether process group pgid still has a member (signal-0 probe of the group).
func GroupAlive(pgid int) bool { return pgid > 1 && Alive(-pgid) }
