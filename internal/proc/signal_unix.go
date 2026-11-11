//go:build unix

package proc

import "syscall"

// sysKill is the signal syscall; tests swap it to observe targets without signalling.
var sysKill = syscall.Kill
