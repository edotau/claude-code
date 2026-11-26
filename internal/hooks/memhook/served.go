package memhook

import (
	"os"
	"time"

	"github.com/edotau/claude-code/internal/dev/preflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// refreshServed re-lists a stale in-use served listing detached when served routing is on; never from an update child.
func refreshServed(reg *providers.Registry, pins []preflight.Pin, cwd string, now time.Time) {
	if !memory.IsUpdateChild() && providers.ServedRoutingOn() && reg != nil && preflight.Stale(reg, pins, now) && preflight.Claim(now) {
		spawnServed(cwd)
	}
}

// spawnServed is the detached `models preflight --refresh` seam; tests replace it.
var spawnServed = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	return proc.SpawnDetached(self, []string{"models", "preflight", "--refresh"}, cwd, null)
}
