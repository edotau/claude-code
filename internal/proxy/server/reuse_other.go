//go:build !darwin && !linux

package server

import "syscall"

// reusePort is a no-op here: a replacement waits for the old daemon's port to free.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }
