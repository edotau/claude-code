package providers

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// tokenExpirySchema is the token-expiry record's schema version; a mismatch is a cache miss.
const tokenExpirySchema = 1

var tokenExpiryFormat = schemas.Register(schemas.Format{
	Name: "token-expiry", Owner: "internal/providers", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "cache", "token-expiry", "*.json") },
	New:     func() any { return &tokenExpiryRecord{} },
	Version: tokenExpirySchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
