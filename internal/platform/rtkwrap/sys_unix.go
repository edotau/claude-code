//go:build unix

package rtkwrap

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// sessionID is this process's session id; Pi spawns its tool shell detached, so the shell leads the session.
func sessionID() int {
	sid, err := unix.Getsid(0)
	if err != nil {
		return -1
	}
	return sid
}

func execve(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }
