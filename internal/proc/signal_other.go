//go:build !unix

package proc

import (
	"errors"
	"syscall"
)

// sysKill is unsupported off unix: no process-group signals, so signalling by pid fails.
var sysKill = func(int, syscall.Signal) error {
	return errors.New("signalling a process by pid is unsupported on this platform")
}
