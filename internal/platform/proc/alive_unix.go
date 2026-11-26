//go:build unix

package proc

import "syscall"

// Alive reports whether pid is a live process (signal-0 probe). EPERM means alive but not ours.
func Alive(pid int) bool { return pid > 0 && probe(pid) }

// GroupAlive reports whether process group pgid still has a member (signal-0 probe of the group).
func GroupAlive(pgid int) bool { return pgid > 1 && probe(-pgid) }

// probe sends signal 0 to target (a pid, or -pgid for a group); EPERM means alive but not ours.
func probe(target int) bool {
	err := syscall.Kill(target, 0)
	return err == nil || err == syscall.EPERM
}
