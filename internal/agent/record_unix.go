//go:build unix

package agent

import "golang.org/x/sys/unix"

// processGroup is pid's process group id, 0 when unknown.
func processGroup(pid int) int {
	if pid <= 0 {
		return 0
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil || pgid <= 0 {
		return 0
	}
	return pgid
}
