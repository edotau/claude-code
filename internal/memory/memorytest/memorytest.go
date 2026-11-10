// Package memorytest stubs memory's detached worker spawns so no test relaunches the test binary.
package memorytest

import (
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// StubSpawn replaces every detached worker spawn (update ok-flagged, the rest succeed) until t ends.
func StubSpawn(t testing.TB, ok bool) *memory.Spawns {
	t.Helper()
	s, restore := memory.StubSpawns(ok)
	t.Cleanup(restore)
	return s
}
