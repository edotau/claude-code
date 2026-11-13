package catalog

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// targets resolves the probe set: every served chat endpoint (all), else ids (default: defaults) against served.
func targets(ids []string, all bool, served map[string]providers.ModelInfo, p *providers.Provider, defaults []string,
) []serving.CheckTarget {
	var out []serving.CheckTarget
	if all {
		for _, name := range slices.Sorted(maps.Keys(served)) {
			if served[name].Task != "llm/v1/chat" {
				continue
			}
			id := ID(name)
			out = append(out, serving.CheckTarget{ID: id, Wire: name, Responses: providers.ResponsesID(id)})
		}
		return out
	}
	if len(ids) == 0 {
		ids = defaults
	}
	for _, raw := range ids {
		id := ID(raw)
		t := serving.CheckTarget{ID: id, Responses: providers.ResponsesID(id)}
		if _, ok := served[p.ModelPrefix+id]; ok {
			t.Wire = p.ModelPrefix + id
		} else if _, ok := served[id]; ok {
			t.Wire = id
		} else {
			t.Missing = true
		}
		out = append(out, t)
	}
	return out
}

// check probes targets on p, sorts the results and merges them into the cache; cacheErr is a cache write failure.
func check(p *providers.Provider, targets []serving.CheckTarget, o serving.CheckOptions,
) (results []serving.CheckResult, cacheErr, err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err = serving.Probe(ctx, p, targets, o)
	if err != nil { // an interrupted run is neither cached nor reported as success
		return nil, nil, err
	}
	sortCheckResults(results)
	now := checkNow().UTC()
	for i := range results {
		results[i].CheckedAt = now
	}
	return results, saveMerged(p.Name, results, now), nil
}

// saveMerged merges results into the cache under its lock, so concurrent runs keep each other's rows.
func saveMerged(profile string, results []serving.CheckResult, now time.Time) error {
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(CheckCachePath(), func() error {
		return SaveCheckCache(CheckCache{CheckedAt: now, Provider: profile, Results: merged(profile, results)})
	})
}

// checkNow is the check cache's clock.
var checkNow = time.Now

// checkAgeAfter is the age past which checkCell shows how old a cached verdict is.
const checkAgeAfter = 6 * time.Hour

// merged lays results over the profile's cached ones by id (a transient never replaces an ok), sanitized.
func merged(profile string, results []serving.CheckResult) []serving.CheckResult {
	byID, _ := LoadCheckCache(profile)
	if byID == nil {
		byID = map[string]serving.CheckResult{}
	}
	for _, r := range results {
		if r.Status == serving.StatusTransient && byID[r.ID].Status == serving.StatusOK {
			continue
		}
		byID[r.ID] = r
	}
	out := serving.SanitizeChecks(slices.Collect(maps.Values(byID)))
	sortCheckResults(out)
	return out
}

// WriteCheck renders probe results as the ID / STATUS / HTTP / LATENCY / TOOLS table, or JSON.
func WriteCheck(w io.Writer, results []serving.CheckResult, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(w, results)
	}
	idW := ColWidth("ID", len(results), func(i int) string { return results[i].ID })
	fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s\n", idW, "ID", "STATUS", "HTTP", "LATENCY", "TOOLS")
	for _, r := range results {
		code, lat := "-", "-"
		if r.HTTP > 0 {
			code = strconv.Itoa(r.HTTP)
		}
		if r.Status != "missing" {
			lat = fmt.Sprintf("%dms", r.LatencyMS)
		}
		fmt.Fprintf(w, "%-*s  %-9s  %-4s  %-10s  %s", idW, r.ID, r.Status, code, lat, cmp.Or(r.Tools, "-"))
		if (r.Status == serving.StatusFail || r.Status == serving.StatusTransient) && r.Error != "" {
			fmt.Fprint(w, "  "+r.Error)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// ColWidth is the widest of the head and the n cells.
func ColWidth(head string, n int, cell func(int) string) int {
	w := len(head)
	for i := range n {
		w = max(w, len(cell(i)))
	}
	return w
}

// checkCell renders a cached probe as "ok tools", "ok no-tools", "fail 400", "missing" or "-", aged past 6h: "(3d)".
func checkCell(cache map[string]serving.CheckResult, id string) string {
	r, ok := cache[id]
	if !ok {
		return "-"
	}
	cell := r.Status
	switch {
	case r.Status == serving.StatusOK && r.Tools == "no":
		cell = "ok no-tools"
	case r.Status == serving.StatusOK:
		cell = "ok tools"
	case (r.Status == serving.StatusFail || r.Status == serving.StatusTransient) && r.HTTP > 0:
		cell = fmt.Sprintf("%s %d", r.Status, r.HTTP)
	}
	if age := checkNow().Sub(r.CheckedAt); !r.CheckedAt.IsZero() && age > checkAgeAfter {
		return cell + " (" + ageLabel(age) + ")"
	}
	return cell
}

// ageLabel is an age in whole hours under a day, else whole days.
func ageLabel(age time.Duration) string {
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}

// checkFamily is the id's leading letters, e.g. claude, gpt, gemini, glm, qwen.
func checkFamily(id string) string {
	id = stripSpellingPrefix(id)
	if i := strings.IndexFunc(id, func(r rune) bool { return !unicode.IsLetter(r) }); i > 0 {
		return id[:i]
	}
	return id
}

// sortCheckResults orders by family, then latency (non-ok last in family), then id.
func sortCheckResults(rs []serving.CheckResult) {
	slices.SortStableFunc(rs, func(a, b serving.CheckResult) int {
		if c := cmp.Compare(checkFamily(a.ID), checkFamily(b.ID)); c != 0 {
			return c
		}
		la, lb := a.LatencyMS, b.LatencyMS
		if a.Status != serving.StatusOK {
			la = 1 << 60
		}
		if b.Status != serving.StatusOK {
			lb = 1 << 60
		}
		return cmp.Or(cmp.Compare(la, lb), cmp.Compare(a.ID, b.ID))
	})
}

// CheckUsable probes ids, else every chat endpoint in infos (all), else those usable on dialects, on client routes.
func CheckUsable(p *providers.Provider, infos []providers.ModelInfo, dialects, ids []string, all bool, timeout time.Duration,
) (results []serving.CheckResult, cacheErr, err error) {
	var usableIDs []string
	for _, info := range UsableIn(p, infos, dialects) {
		usableIDs = append(usableIDs, info.ID)
	}
	return check(p, targets(ids, all, byName(infos), p, usableIDs),
		serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: timeout})
}

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
	return paths.WriteJSON(CheckCachePath(), c)
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
const checkCacheSchema = 2

// checkCacheFormat registers the shared model check cache's on-disk format.
var checkCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-models-check", Owner: "internal/catalog", Kind: schemas.Cache,
	Glob:    func() string { return CheckCachePath() },
	New:     func() any { return &CheckCache{} },
	Version: checkCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
