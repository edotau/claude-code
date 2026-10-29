package cli

import "github.com/edotau/claude-code/internal/schemas"

// dispatchCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const dispatchCacheSchema = 1

var dispatchCacheFormat = schemas.Register(schemas.Format{
	Name: "tailscale-machines", Owner: "internal/cli", Kind: schemas.Cache,
	Glob:    func() string { return dispatchCachePath() },
	New:     func() any { return &dispatchMachineCache{} },
	Version: dispatchCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// decodeDispatchCache reports whether raw decodes at the current schema; older, newer or unstamped data is a miss.
func decodeDispatchCache(raw []byte, dst any) bool {
	_, err := schemas.Decode(dispatchCacheFormat, raw, dst)
	return err == nil
}
