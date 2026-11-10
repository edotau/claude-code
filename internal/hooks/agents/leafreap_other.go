//go:build !unix

package agents

import (
	"errors"
	"syscall"
)

// reapSupported: no POSIX process groups here, so reapLeaf stays a no-op.
const reapSupported = false

// errNoGroups is what the group seams return off unix; they only satisfy the build.
var errNoGroups = errors.New("process groups are unsupported on this platform")

var (
	reapOwnGroup = func() int { return 0 }
	reapGroupOf  = func(int) (int, error) { return 0, errNoGroups }
	reapSignal   = func(int, syscall.Signal) error { return errNoGroups }
)
