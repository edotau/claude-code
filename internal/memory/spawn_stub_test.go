package memory

import "testing"

// StubSpawn is memtest.StubSpawn for this package's own tests, which cannot import memtest (cycle).
func StubSpawn(tb testing.TB, ok bool) *Spawns {
	tb.Helper()
	s, restore := StubSpawns(ok)
	tb.Cleanup(restore)
	return s
}
