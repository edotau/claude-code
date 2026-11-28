// Package catalog lists, probes and discovers a Databricks profile's serving endpoints for every agent's catalog verb.
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

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/schemas"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/databricks/serving"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

// Profile is agent's catalog profile: flag, pin, session, else its own login row; nil when none is a row.
func Profile(ctx context.Context, agent, flag string) (*providers.Provider, error) {
	pin := vault.GetAgentRecord(agent).EffectiveRoute()
	if pin.Provider == providers.GroupDatabricks && pin.Profile == route.AutoArm { // the pool names no workspace
		pin.Profile = ""
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	name := cmp.Or(flag, pin.Profile, providers.SessionProfileContext(ctx))
	if own := vault.OwnLoginProfiles[agent]; name == "" && reg.Row(own) != nil { // no config: first party
		name = own
	}
	if name == "" {
		return nil, nil
	}
	return reg.Get(name)
}

// Served lists p's endpoints: the sealed snapshot when remembered (hit, with its time), else a live listing under ctx.
func Served(ctx context.Context, p *providers.Provider) (
	infos []providers.ModelInfo, discoveredAt time.Time, hit bool, err error) {
	snap, ok := recallSnapshot(ctx, p, func(m []providers.ModelInfo) bool { return len(m) > 0 },
		func(profile string) bool { return SpawnDiscover(profile, false) })
	if ok {
		return snap.Models, snap.DiscoveredAt, true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if infos, err = providers.ListModels(ctx, p); err != nil {
		return nil, time.Time{}, false, err
	}
	return infos, time.Now().UTC(), false, nil
}

func byName(infos []providers.ModelInfo) map[string]providers.ModelInfo {
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out
}

// servedName is the served endpoint name for id on p: the ModelPrefix spelling first, else the bare id.
func servedName(served map[string]providers.ModelInfo, p *providers.Provider, id string) (string, bool) {
	for _, name := range []string{p.ModelPrefix + id, id} {
		if _, ok := served[name]; ok {
			return name, true
		}
	}
	return "", false
}

func servedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	name, ok := servedName(served, p, id)
	return served[name], ok
}

// UsableIn keeps infos' READY chat endpoints whose wire on p (providers.WireFor) is one of dialects, sorted by name.
func UsableIn(p *providers.Provider, infos []providers.ModelInfo, dialects []string) []providers.ModelInfo {
	served := byName(infos)
	var out []providers.ModelInfo
	for _, name := range slices.Sorted(maps.Keys(served)) {
		info := served[name]
		if !info.ReadyChat() {
			continue
		}
		if wire, ok, _ := providers.WireFor(p, name); ok && slices.Contains(dialects, wire) {
			out = append(out, info)
		}
	}
	return out
}

// ID canonicalizes a typed model id: a catalog id via models.Resolve, else the id minus any spelling prefix.
func ID(id string) string {
	id = strings.TrimSpace(models.Strip1M(id))
	if canonical, _, ok := models.Resolve(id); ok {
		return canonical
	}
	return stripSpellingPrefix(id)
}

// stripSpellingPrefix removes the id prefix a catalog spelling rule adds (derived from models.Wire, not listed here).
func stripSpellingPrefix(id string) string {
	if prefix := strings.TrimSuffix(models.Wire("databricks-serving", "x"), "x"); prefix != "" {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return rest
		}
	}
	return id
}

// Row is one `<agent> catalog list` line: an endpoint the agent can use and its last probe status ("" = never).
type Row struct {
	Name   string `json:"name"`
	Tested string `json:"tested"`
}

// Rows pairs each endpoint name with the last probe status the shared check cache holds for profile.
func Rows(profile string, names []string) []Row {
	cache, _ := LoadCheckCache(profile)
	var rows []Row
	for _, name := range names {
		rows = append(rows, Row{Name: name, Tested: cache[ID(name)].Status})
	}
	return rows
}

// WriteList renders rows under a "<profile> serving endpoints" title as a NAME / TESTED table, or JSON.
func WriteList(w io.Writer, profile string, rows []Row, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(w, rows)
	}
	fmt.Fprintln(w, profile+" serving endpoints")
	nameW := colWidth("NAME", len(rows), func(i int) string { return rows[i].Name })
	fmt.Fprintf(w, "%-*s  %s\n", nameW, "NAME", "TESTED")
	for _, r := range rows {
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("%-*s  %s", nameW, r.Name, r.Tested), " "))
	}
	return nil
}

// Refresh force re-lists p's endpoints and seals the result, which also warms the router's copy.
func Refresh(ctx context.Context, p *providers.Provider) error {
	snapshot, err := serving.Discover(ctx, p, true)
	if err != nil {
		return err
	}
	_, err = Seal(p, snapshot, false)
	return err
}

// Seal stores snapshot in the vault when it is new or freshly verified, then copies a live listing to the router.
func Seal(p *providers.Provider, snapshot serving.Snapshot, verified bool) (sealed bool, err error) {
	if snapshot.CacheHit && !verified {
		return false, nil
	}
	if err := serving.Save(p, snapshot); err != nil {
		return false, err
	}
	if snapshot.CacheHit {
		return true, nil
	}
	if err := serving.WarmServed(p.Name, snapshot.Models, snapshot.DiscoveredAt); err != nil {
		return true, fmt.Errorf("served listing not saved: %w", err)
	}
	return true, nil
}

// SyncSummaryLines are the discovery and verification lines that head a sync report.
func SyncSummaryLines(snapshot serving.Snapshot, verified bool) []string {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	lines := []string{fmt.Sprintf("Valid identity (not endpoint permissions); %s, discovered %s",
		source, snapshot.DiscoveredAt.Format(time.RFC3339))}
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		return append(lines, "Verification: listing only; models are discovered/unverified")
	}
	return append(lines,
		fmt.Sprintf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted",
			label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools),
		"Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}

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
		wire, ok := servedName(served, p, id)
		t.Wire, t.Missing = wire, !ok
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
	idW := colWidth("ID", len(results), func(i int) string { return results[i].ID })
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
		if (r.Status == serving.StatusFail || r.Status == serving.StatusTransient || r.Status == serving.StatusBroken) && r.Error != "" {
			fmt.Fprint(w, "  "+r.Error)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// colWidth is the widest of the head and the n cells.
func colWidth(head string, n int, cell func(int) string) int {
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

func ageLabel(age time.Duration) string {
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}

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
	return paths.WriteJSONNoSync(CheckCachePath(), c)
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

var checkCacheFormat = schemas.Register(schemas.Format{
	Name: "opencode-models-check", Owner: "internal/routing/catalog", Kind: schemas.Cache,
	Glob:    func() string { return CheckCachePath() },
	New:     func() any { return &CheckCache{} },
	Version: checkCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
