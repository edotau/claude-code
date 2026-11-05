package memhook

import "testing"

// StubSpawnForTest is memhooktest.StubSpawn for this package's own tests (memhooktest imports memhook).
func StubSpawnForTest(t testing.TB, ok bool) *Spawns {
	s, restore := StubSpawns(ok)
	t.Cleanup(restore)
	return s
}
