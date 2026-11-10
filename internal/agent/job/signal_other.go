//go:build !unix

package job

import (
	"errors"
	"syscall"
)

// sysKill is unsupported off unix: no process-group signals, so cancelling a job by pid fails.
var sysKill = func(int, syscall.Signal) error {
	return errors.New("signalling a job by pid is unsupported on this platform")
}
