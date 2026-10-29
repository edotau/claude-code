package usage

import "github.com/edotau/claude-code/internal/schemas"

// usageCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const usageCacheSchema = 1

var usageCacheFormat = schemas.Register(schemas.Format{
	Name: "oauth-usage-cache", Owner: "internal/usage", Kind: schemas.Cache,
	Glob:    func() string { return claudeUsageCachePath() },
	New:     func() any { return &claudeUsageCache{} },
	Version: usageCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
