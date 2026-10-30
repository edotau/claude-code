//go:build unix

package vscode

import "syscall"

// terminate signals the whole process group, not just pid: spawn's proc.Detach (Setsid) makes pid
// its own group leader, but `code` is a shell wrapper several forks deep from the real serve-web
// server, so signalling only pid would orphan that server instead of stopping it.
func terminate(pid int) error { return syscall.Kill(-pid, syscall.SIGTERM) }
