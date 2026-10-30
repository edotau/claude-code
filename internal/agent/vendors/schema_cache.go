package vendors

import "github.com/edotau/claude-code/internal/schemas"

// opencodeCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const opencodeCacheSchema = 1

var opencodeCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-auth-cache", Owner: "internal/agent/vendors", Kind: schemas.Cache,
	Glob:    func() string { return opencodeCacheFile() },
	New:     func() any { return &opencodeDiskCache{} },
	Version: opencodeCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
