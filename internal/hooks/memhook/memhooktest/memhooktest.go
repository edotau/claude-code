// Package memhooktest stubs memhook's detached spawns so no test relaunches the test binary.
package memhooktest

import (
	"testing"

	"github.com/edotau/claude-code/internal/hooks/memhook"
)

// StubSpawn replaces every detached spawn (update ok-flagged, the rest succeed) until t ends.
func StubSpawn(t testing.TB, ok bool) *memhook.Spawns {
	t.Helper()
	s, restore := memhook.StubSpawns(ok)
	t.Cleanup(restore)
	return s
}
