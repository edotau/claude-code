package memhook

import "sync/atomic"

// Spawns counts the stubbed spawns tests read; atomic because the idle watcher calls its stub on its own goroutine.
type Spawns struct{ Update, Dream, Catchup atomic.Int64 }

// StubSpawns replaces every detached spawn (update ok-flagged, the rest succeed); restore undoes it.
// Tests reach it through memhooktest so the binary never imports "testing".
func StubSpawns(ok bool) (s *Spawns, restore func()) {
	s = new(Spawns)
	oU, oW, oD, oC, oS := spawnUpdate, spawnWatcher, spawnDream, spawnCatchup, spawnServed
	spawnUpdate = func(string, string, string) bool { s.Update.Add(1); return ok }
	spawnWatcher = func([]string, string) bool { return true }
	spawnDream = func(string) bool { s.Dream.Add(1); return true }
	spawnCatchup = func(string, string) bool { s.Catchup.Add(1); return true }
	spawnServed = func(string) bool { return true }
	return s, func() { spawnUpdate, spawnWatcher, spawnDream, spawnCatchup, spawnServed = oU, oW, oD, oC, oS }
}
