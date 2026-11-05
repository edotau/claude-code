package memory

import "sync/atomic"

// StubSpawnForTest replaces the detached update, idle-watcher and dream spawns so no test starts a real process.
// The counter is atomic: the idle watcher runs the stub on its own goroutine while the test polls.
func StubSpawnForTest(t interface {
	Helper()
	Cleanup(func())
}, ok bool) *atomic.Int64 {
	t.Helper()
	calls := new(atomic.Int64)
	orig, origWatch, origDream := spawnUpdate, spawnWatcher, spawnDream
	spawnUpdate = func(string, string, string) bool { calls.Add(1); return ok }
	spawnWatcher = func([]string, string) bool { return true }
	spawnDream = func(string) bool { return true }
	t.Cleanup(func() { spawnUpdate, spawnWatcher, spawnDream = orig, origWatch, origDream })
	return calls
}
