package tailscale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// machinesCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const machinesCacheSchema = 1

var machinesCacheFormat = schemas.Register(schemas.Format{
	Name: "tailscale-machines", Owner: "internal/network/tailscale", Kind: schemas.Cache,
	Glob:    func() string { return machinesCachePath() },
	New:     func() any { return &machinesCache{} },
	Version: machinesCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// machinesCachePath holds the last --machines listing for completion, which never calls the network.
func machinesCachePath() string { return filepath.Join(paths.StateDir(), "tailscale-machines.json") }

// machinesCacheTTL bounds how stale the cached machine list may be before completion ignores it.
const machinesCacheTTL = 10 * time.Minute

type machinesCache struct {
	Schema int       `json:"schema"`
	At     time.Time `json:"at"`
	Names  []string  `json:"names"`
}

// WriteMachinesCache records a successful --machines listing; best-effort, errors are dropped.
func WriteMachinesCache(names []string) {
	data, err := json.Marshal(machinesCache{Schema: machinesCacheSchema, At: time.Now(), Names: names})
	if err != nil {
		return
	}
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	_ = paths.AtomicWrite(machinesCachePath(), data, 0o600)
}

// CachedMachineNames reads the --machines cache when fresh, else nil: Tab completion has no budget for calls.
func CachedMachineNames() []string {
	data, err := os.ReadFile(machinesCachePath())
	if err != nil {
		return nil
	}
	var c machinesCache
	if !schemas.DecodeOK(machinesCacheFormat, data, &c) {
		return nil
	}
	if age := time.Since(c.At); age < 0 || age > machinesCacheTTL { // future At (clock skew) is stale
		return nil
	}
	return c.Names
}
