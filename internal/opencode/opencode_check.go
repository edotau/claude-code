package opencode

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// CheckFamily is the id's leading letters, e.g. claude, gpt, gemini, glm, qwen.
func CheckFamily(id string) string {
	id = stripSpellingPrefix(id)
	if i := strings.IndexFunc(id, func(r rune) bool { return !unicode.IsLetter(r) }); i > 0 {
		return id[:i]
	}
	return id
}

// SortCheckResults orders by family, then latency (non-ok last in family), then id.
func SortCheckResults(rs []serving.CheckResult) {
	slices.SortStableFunc(rs, func(a, b serving.CheckResult) int {
		if c := cmp.Compare(CheckFamily(a.ID), CheckFamily(b.ID)); c != 0 {
			return c
		}
		la, lb := a.LatencyMS, b.LatencyMS
		if a.Status != "ok" {
			la = 1 << 60
		}
		if b.Status != "ok" {
			lb = 1 << 60
		}
		return cmp.Or(cmp.Compare(la, lb), cmp.Compare(a.ID, b.ID))
	})
}

// CheckCache is the persisted result set, scoped to one provider.
type CheckCache struct {
	Schema    int                   `json:"schema"`
	CheckedAt time.Time             `json:"checked_at"`
	Provider  string                `json:"provider"`
	Results   []serving.CheckResult `json:"results"`
}

// CheckCachePath is state/harness/opencode-models-check.json.
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

var checkCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-models-check", Owner: "internal/opencode", Kind: schemas.Cache,
	Glob:    func() string { return CheckCachePath() },
	New:     func() any { return &CheckCache{} },
	Version: checkCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
