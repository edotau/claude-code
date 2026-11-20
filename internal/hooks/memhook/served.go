package memhook

import (
	"os"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// refreshServed re-lists a stale served-endpoint cache detached; needs no memory bank, never from an update child.
func refreshServed(reg *providers.Registry, pins []preflight.Pin, cwd string, now time.Time) {
	if !memory.IsUpdateChild() && servedRefreshDue(reg, pins, now) {
		spawnServed(cwd)
	}
}

// servedRefreshDue reports whether served routing is on and an in-use Databricks profile has no fresh listing.
func servedRefreshDue(reg *providers.Registry, pins []preflight.Pin, now time.Time) bool {
	if !providers.ServedRoutingOn() {
		return false
	}
	return reg != nil && preflight.Stale(reg, pins, now) && preflight.Claim(now)
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
