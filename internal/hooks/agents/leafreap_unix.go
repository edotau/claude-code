//go:build unix

package agents

import "syscall"

// Seams for tests: our own group, a pid's group probe and the group signaller.
var (
	reapOwnGroup = syscall.Getpgrp
	reapGroupOf  = syscall.Getpgid
	reapSignal   = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
)
