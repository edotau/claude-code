package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// CheckCache is the last check run's results, scoped to one profile and shared by every agent's catalog.
type CheckCache struct {
	Schema    int                   `json:"schema"`
	CheckedAt time.Time             `json:"checked_at"`
	Provider  string                `json:"provider"`
	Results   []serving.CheckResult `json:"results"`
}

// CheckCachePath is state/harness/opencode-models-check.json (the name predates its sharing by every agent).
func CheckCachePath() string { return filepath.Join(paths.StateDir(), "opencode-models-check.json") }

// SaveCheckCache writes c (no secrets) with mode 0600.
func SaveCheckCache(c CheckCache) error {
	c.Schema = checkCacheSchema
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := CheckCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(path, append(b, '\n'), 0o600)
}

// LoadCheckCache returns the cached results by id when the cache exists and belongs to provider.
func LoadCheckCache(provider string) (map[string]serving.CheckResult, bool) {
	b, err := os.ReadFile(CheckCachePath())
	if err != nil {
		return nil, false
	}
	var c CheckCache
	if !schemas.DecodeOK(checkCacheFormat, b, &c) || c.Provider != provider {
		return nil, false
	}
	m := make(map[string]serving.CheckResult, len(c.Results))
	for _, r := range c.Results {
		m[r.ID] = r
	}
	return m, true
}

// checkCacheSchema is the cache's current schema version; a mismatch is a cache miss, never migrated.
const checkCacheSchema = 1

// checkCacheFormat registers the shared model check cache's on-disk format.
var checkCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-models-check", Owner: "internal/catalog", Kind: schemas.Cache,
	Glob:    func() string { return CheckCachePath() },
	New:     func() any { return &CheckCache{} },
	Version: checkCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
