package memhook

import (
	"sync/atomic"
	"testing"
)

// Spawns counts the stubbed detached spawns; atomic because the idle watcher calls its stub on its own goroutine.
type Spawns struct{ Update, Watch, Dream, Catchup, Served atomic.Int64 }

// StubSpawnForTest replaces every detached spawn (update ok-flagged, the rest succeed) so no test relaunches the test binary.
func StubSpawnForTest(t testing.TB, ok bool) *Spawns {
	t.Helper()
	s := new(Spawns)
	oU, oW, oD, oC, oS := spawnUpdate, spawnWatcher, spawnDream, spawnCatchup, spawnServed
	spawnUpdate = func(string, string, string) bool { s.Update.Add(1); return ok }
	spawnWatcher = func([]string, string) bool { s.Watch.Add(1); return true }
	spawnDream = func(string) bool { s.Dream.Add(1); return true }
	spawnCatchup = func(string, string) bool { s.Catchup.Add(1); return true }
	spawnServed = func(string) bool { s.Served.Add(1); return true }
	t.Cleanup(func() { spawnUpdate, spawnWatcher, spawnDream, spawnCatchup, spawnServed = oU, oW, oD, oC, oS })
	return s
}
