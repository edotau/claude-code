//go:build !linux && !darwin

package hosttest

import (
	"os"
	"testing"
)

// PTY skips: this platform has no pseudo-terminal support here.
func PTY(t testing.TB) (master, slave *os.File) {
	t.Helper()
	t.Skip("no pseudo-terminal on this platform")
	return nil, nil
}
