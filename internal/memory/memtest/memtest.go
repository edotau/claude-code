// Package memtest is test support for code that drives the memory package.
package memtest

import (
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// StubSpawn stubs every detached worker spawn until tb ends, so no test relaunches the test binary.
func StubSpawn(tb testing.TB, ok bool) *memory.Spawns {
	tb.Helper()
	s, restore := memory.StubSpawns(ok)
	tb.Cleanup(restore)
	return s
}
