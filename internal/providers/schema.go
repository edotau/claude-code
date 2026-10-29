package providers

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// checkCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const checkCacheSchema = 1

var checkCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-models-check", Owner: "internal/providers", Kind: schemas.Cache,
	Glob:    func() string { return CheckCachePath() },
	New:     func() any { return &CheckCache{} },
	Version: checkCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// decodeCheckCache reports whether raw decodes at the current schema; older, newer or unstamped data is a miss.
func decodeCheckCache(raw []byte, dst any) bool {
	_, err := schemas.Decode(checkCacheFormat, raw, dst)
	return err == nil
}

// tokenExpirySchema is the token-expiry record's schema version; a mismatch is a cache miss.
const tokenExpirySchema = 1

var tokenExpiryFormat = schemas.Register(schemas.Format{
	Name: "token-expiry", Owner: "internal/providers", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "cache", "token-expiry", "*.json") },
	New:     func() any { return &tokenExpiryRecord{} },
	Version: tokenExpirySchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
