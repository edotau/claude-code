package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/proxyteam"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

// ancestryOff keeps tests off the live process tree, which may sit under a real dispatch.
var ancestryOff = testing.Testing()

// ancestorDispatch is the nearest running dispatch above this process, looked up once per process.
var ancestorDispatch = sync.OnceValues(findAncestorDispatch)

// CheckAncestorDispatch refuses an env-wiped ask under a shared or finished ancestor dispatch.
func CheckAncestorDispatch(ctx context.Context) error {
	if DispatchIDOf(ctx) != "" || proc.Value(ctx, proc.DispatchIDEnv) != "" {
		return nil
	}
	_, err := ancestorDispatch()
	return err
}

// AncestorContext installs the values an env-wiped ask recovers, so route fences read the parent's, not the ambient.
func AncestorContext(ctx context.Context) context.Context {
	if _, ok := proc.ValuesFromContext(ctx); ok || DispatchIDOf(ctx) != "" || proc.Value(ctx, proc.DispatchIDEnv) != "" {
		return ctx
	}
	if rec, err := ancestorDispatch(); err != nil || rec.ID == "" {
		return ctx
	}
	ctx, _ = RunValues(ctx, nil)
	return ctx
}

// withAncestorDispatch fills a dispatch env a leaf wiped from the nearest running dispatch above it; a set id wins.
func withAncestorDispatch(values proc.Values) proc.Values {
	if values[proc.DispatchIDEnv] != "" {
		return values
	}
	rec, err := ancestorDispatch()
	if err != nil || rec.ID == "" {
		return values
	}
	set := proc.Values{proc.DispatchIDEnv: rec.ID, proc.DispatchDepthEnv: strconv.Itoa(rec.Depth)}
	parent, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{})
	if err == nil {
		parent, err = DeriveUnit(parent, agentconfig.Overlay{}, agentconfig.HopDispatchChild)
	}
	own := parent
	if err != nil { // fail closed: the parent's unit under a fence that admits no profile
		own = agentconfig.Overlay{Provider: rec.Route.Provider, Profile: rec.Route.Profile, Fence: &route.Fence{}}
	}
	if doc := childDoc(own); doc != "" {
		set[route.OverlayVar] = doc // the parent's fence beats any ambient one
	}
	return values.Merge(set) // an env cap already in values still tightens: dispatchLimit takes the smaller
}

// findAncestorDispatch recovers a unique nearest live dispatch or refuses a shared or finished parent.
func findAncestorDispatch() (Record, error) {
	if ancestryOff {
		return Record{}, nil
	}
	chain := dispatchAncestors()
	if len(chain) == 0 { // top level: no ancestor runs the harness binary, so no record is read
		return Record{}, nil
	}
	best, err := nearestDispatch(ancestorRecords(paths.DispatchDir(), chain), chain)
	if best.ID != "" {
		fmt.Fprintf(NoticeOut, "claude-code: no %s in the env; parent dispatch %s found by process ancestry\n", proc.DispatchIDEnv, best.ID)
	}
	return best, err
}

// ancestorRecords is every record, finished included, whose pid is in chain, found through the .children.idx cache.
func ancestorRecords(dir string, chain map[int]int) []Record {
	// unlocked, since the index is renamed into place whole; the lock covers only the merge, so admission never waits
	next, recs, changed := scanAncestors(dir, chain)
	if changed {
		_ = paths.WithFileLockWithin(filepath.Join(dir, ".children"), dispatchChildrenLockWait, func() error {
			indexedAncestors(dir, next)
			return nil
		}) // a lock failure only skips the cache write
	}
	return recs
}

// scanAncestors is the index with every record's pid and the records whose pid is in chain, decoding only those.
func scanAncestors(dir string, chain map[int]int) (map[string]childEntry, []Record, bool) {
	old := readChildIndex(dir)
	names, _ := recordNames(dir) // unreadable records fail open, like dispatch history
	next := make(map[string]childEntry, len(names))
	changed := false
	var recs []Record
	for _, n := range names {
		key := strings.TrimSuffix(n, ".json")
		e, rec, ok := pidEntry(filepath.Join(dir, n), old[key], chain)
		if !ok {
			continue
		}
		changed = changed || e != old[key]
		next[key] = e
		if _, hit := chain[rec.PID]; hit && rec.ID != "" {
			recs = append(recs, rec)
		}
	}
	return next, recs, changed
}

func readChildIndex(dir string) map[string]childEntry {
	idx := map[string]childEntry{}
	if b, err := os.ReadFile(filepath.Join(dir, childIndexFile)); err == nil {
		_ = json.Unmarshal(b, &idx) // a bad index is rebuilt from the records
	}
	return idx
}

// indexedAncestors merges a walk's entries into the index, keeping an entry whose record changed since the walk.
func indexedAncestors(dir string, next map[string]childEntry) {
	cur := readChildIndex(dir)
	changed := false
	for k, e := range next {
		c, ok := cur[k]
		if c == e || ok && (c.Mod != e.Mod || c.Size != e.Size) && !statIs(filepath.Join(dir, k+".json"), e) {
			continue
		}
		cur[k], changed = e, true
	}
	if !changed {
		return
	}
	if b, err := json.Marshal(cur); err == nil { // admission's own scan prunes deleted records
		_ = paths.AtomicWriteNoSync(filepath.Join(dir, childIndexFile), b, 0o600) // a cache: a failed write costs a reread
	}
}

func statIs(path string, e childEntry) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.ModTime().UnixNano() == e.Mod && fi.Size() == e.Size
}

// pidEntry is path's index entry with its pid, decoding the record when the cache lacks a pid or its pid is in chain.
func pidEntry(path string, cached childEntry, chain map[int]int) (childEntry, Record, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return childEntry{}, Record{}, false
	}
	e := childEntry{Mod: fi.ModTime().UnixNano(), Size: fi.Size()}
	if _, hit := chain[cached.PID]; !hit && cached.PID != 0 && cached.Mod == e.Mod && cached.Size == e.Size {
		return cached, Record{}, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return childEntry{}, Record{}, false
	}
	rec, err := decodeRecord(b)
	if err != nil {
		return e, Record{}, true
	}
	if rec.Parent != "" && rec.Status == "running" { // the admission fields childEntryFor caches
		e.Parent, e.Status, e.Time = rec.Parent, rec.Status, rec.Time
	}
	e.PID = rec.PID // admission counts only running children, so a pid on every entry leaves its count unchanged
	return e, rec, true
}

func nearestDispatch(recs []Record, chain map[int]int) (Record, error) {
	best, nearest := Record{}, -1
	ambiguous := false
	for _, r := range recs {
		n, ok := chain[r.PID]
		// a waves coordinator holds no leaf policy or route: its asks are top-level
		if !ok || r.Agent == wavesJobAgent || (nearest >= 0 && n > nearest) || !liveAs(r.PID, r.PIDStart) {
			continue
		}
		if nearest == n {
			ambiguous = true
		} else {
			best, nearest = r, n
			ambiguous = r.Status != "running" || r.BatchID != ""
		}
	}
	if ambiguous {
		return Record{}, fmt.Errorf("cannot tell which dispatch of %d this run belongs to after its dispatch env was cleared; run it with the env intact", best.PID)
	}
	return best, nil
}

func minDispatchCap(a, b *int, fallback int) int {
	x, y := fallback, fallback
	if a != nil && *a >= 0 {
		x = *a
	}
	if b != nil && *b >= 0 {
		y = *b
	}
	if x == 0 || y > 0 && y < x {
		return y
	}
	return x
}

// dispatchExe is the binary every dispatch record's pid runs: the ask process or its --job child.
const dispatchExe = "claude-code"

// dispatchAncestors maps each ancestor running dispatchExe to its distance up the parent chain.
func dispatchAncestors() map[int]int {
	chain := map[int]int{}
	for p, n := proc.Parent(os.Getpid()), 0; p > 1 && n < ancestorWalkCap; p, n = proc.Parent(p), n+1 {
		// linux names a replaced binary "<path> (deleted)": a `make install` mid-run must not hide the dispatch
		if strings.TrimSuffix(filepath.Base(proc.Exe(p)), " (deleted)") == dispatchExe {
			chain[p] = n
		}
	}
	return chain
}

// isCarrier: rec is id's, carries (request.carry), runs top level, and its live process is this one's ancestor.
func isCarrier(id string, rec Record) bool {
	return rec.ID == id && rec.Request.Carry && rec.Status == "running" && rec.Depth == 0 && rec.Parent == "" && liveAncestor(rec.PID, rec.PIDStart, id)
}

// ancestorWalkCap bounds the parent walk from this process to the carrier.
const ancestorWalkCap = 64

// liveAncestor: pid (start time start) is on this process's parent chain with no running dispatch's pid between.
func liveAncestor(pid int, start, id string) bool {
	if !liveAs(pid, start) {
		return false
	}
	var between map[int]bool // a direct child reads no record: read only when the walk passes a process
	for p, n := proc.Parent(os.Getpid()), 0; p > 1 && n < ancestorWalkCap; p, n = proc.Parent(p), n+1 {
		if p == pid {
			return true
		}
		if between == nil {
			between = runningPIDsSince(id)
		}
		if between[p] { // a dispatch's descendant is that dispatch's, even with a forged carrier id
			return false
		}
	}
	return false
}

// liveAs reports pid (never pid 1) alive with start time start.
func liveAs(pid int, start string) bool {
	if pid <= 1 || start == "" || !proc.Alive(pid) {
		return false
	}
	got, err := proc.StartTime(pid)
	return err == nil && got == start
}

// runningPIDsSince is the pids of running records stamped in or after id's millisecond, id's own excluded.
func runningPIDsSince(id string) map[int]bool {
	stamp, _, _ := strings.Cut(id, "-")
	dir := paths.DispatchDir()
	names, _ := recordNames(dir)
	pids := map[int]bool{}
	for _, n := range slices.Backward(names) {
		if n < stamp {
			break
		}
		if r, err := readRecord(filepath.Join(dir, n)); err == nil && r.ID != id && r.Status == "running" && r.PID > 1 {
			pids[r.PID] = true
		}
	}
	return pids
}

// lineage is the depth and cap from the parent's record; the env may tighten both, never loosen (cap 0 = none).
func lineage(ctx context.Context, parent string) (depth, maxDepth int) {
	env, set := envDepthCap(ctx)
	if parent == "" {
		return 0, env
	}
	inherited, _ := strconv.Atoi(proc.Value(ctx, proc.DispatchDepthEnv))
	depth, maxDepth = max(inherited, 0)+1, DefaultMaxDispatchDepth
	pr, ok := parentRecord(parent)
	switch {
	case !ok: // a reaped or stale parent gets the default cap, never uncapped
		fmt.Fprintf(NoticeOut, "claude-code: parent dispatch %s has no readable record; default depth cap applies\n", parent)
	case isCarrier(parent, pr):
		depth = 0 // a carried call has a direct call's depth
	default:
		depth = max(depth, pr.Depth+1)
	}
	if ok && pr.MaxDepth != nil {
		maxDepth = *pr.MaxDepth
	}
	if set && env > 0 && (maxDepth == 0 || env < maxDepth) {
		maxDepth = env
	}
	return depth, maxDepth
}

// envDepthCap is the tighter of the overlay's maxDepth and HARNESS_DISPATCH_MAX_DEPTH; none is the default, not set.
func envDepthCap(ctx context.Context) (int, bool) {
	if n := dispatchLimit(ctx, maxDispatchDepthEnv, -1); n >= 0 {
		return n, true
	}
	return DefaultMaxDispatchDepth, false
}

// parentRecord is the dispatch record named exactly id, read from disk rather than trusted from the caller's env.
func parentRecord(id string) (Record, bool) {
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return Record{}, false
	}
	rec, err := readRecord(filepath.Join(paths.DispatchDir(), id+".json"))
	return rec, err == nil
}

// StoredOverlay is what a record keeps of its overlay: registry row names and typed limits, never free text.
type StoredOverlay struct {
	Endpoint      string   `json:"endpoint,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	Profile       string   `json:"profile,omitempty"`
	Identity      string   `json:"identity,omitempty"`
	Account       string   `json:"account,omitempty"`
	Model         string   `json:"model,omitempty"`
	Fallback      string   `json:"fallback,omitempty"`
	MaxChildren   *int     `json:"maxChildren,omitempty"`
	MaxTreeTokens *int     `json:"maxTreeTokens,omitempty"`
	MaxDepth      *int     `json:"maxDepth,omitempty"`
	BudgetUSD     *float64 `json:"budgetUSD,omitempty"`
	BudgetCredits *int     `json:"budgetCredits,omitempty"`
	Refused       string   `json:"refused,omitempty"` // the field that failed validation (never its value); a replay refuses with it
}

// UnmarshalJSON reads strictly: an unknown key (a newer binary's restriction) marks the overlay refused, not dropped.
func (s *StoredOverlay) UnmarshalJSON(b []byte) error {
	type plain StoredOverlay
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode((*plain)(s)); err != nil {
		*s = StoredOverlay{Refused: "overlay: unreadable or unknown key"}
	}
	return nil
}

// fields builds StoredOverlay from a record's loose fields for writing and old-record replay.
func (r Record) fields() StoredOverlay {
	u, req := r.replayUnit(cmp.Or(r.Request.EffectiveRoute(), r.Route)), r.Request
	s := StoredOverlay{Endpoint: u.Endpoint, Provider: u.Provider, Profile: u.Profile, Identity: u.Identity, Account: u.Account,
		MaxChildren: r.MaxChildren, MaxTreeTokens: r.MaxTreeTokens, MaxDepth: r.MaxDepth, BudgetUSD: req.BudgetUSD, BudgetCredits: req.BudgetCredits}
	if req.ModelTyped {
		s.Model = req.Model
	}
	if !req.FallbackDefaulted {
		s.Fallback = cmp.Or(req.Fallback, r.FallbackPolicy)
	}
	return s
}

// storeOverlay is rec's overlay to store; a name that fails validation is refused with its field named, not stored.
func storeOverlay(rec Record) *StoredOverlay {
	s := rec.fields()
	if err := s.check(); err != nil {
		return &StoredOverlay{Refused: err.Error()}
	}
	return &s
}

// check validates every name against the registry (loaded only when a row name is set); an error names the field alone.
func (s StoredOverlay) check() error {
	if s.Refused != "" {
		return errors.New(s.Refused)
	}
	var reg *providers.Registry
	if s.Provider != "" || s.Profile != "" || s.Model != "" {
		var err error
		if reg, err = providers.LoadShared(); err != nil {
			return errors.New("overlay: registry unreadable")
		}
	}
	row := reg.Row(s.Profile)
	named := map[string]bool{
		"provider": s.Provider == "" || slices.Contains(providers.ProviderKeys(), s.Provider),
		"profile":  s.Profile == "" || s.Profile == route.AutoArm && s.Provider == "databricks" || row != nil && row.Provider == s.Provider,
		"identity": s.Identity == "" || s.Provider == "databricks" && slices.Contains([]string{providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP}, s.Identity),
		"account":  s.Account == "" || s.Provider == "github" && row != nil && row.Login == s.Account,
		"endpoint": s.endpointOK(row),
		"model":    s.modelOK(reg),
		"fallback": s.Fallback == "" || slices.Contains(route.FallbackModes, s.Fallback),
		"limits": !slices.ContainsFunc([]*int{s.MaxChildren, s.MaxTreeTokens, s.MaxDepth, s.BudgetCredits}, func(n *int) bool { return n != nil && *n < 0 }) &&
			(s.BudgetUSD == nil || *s.BudgetUSD >= 0 && !math.IsInf(*s.BudgetUSD, 0)),
	}
	for _, k := range slices.Sorted(maps.Keys(named)) {
		if !named[k] {
			return errors.New("overlay " + k + ": not a registry name or typed limit")
		}
	}
	return nil
}

// endpointOK: no endpoint, a short name, or a URL exactly as the profile's row holds it (a path is never cut or added).
func (s StoredOverlay) endpointOK(row *providers.Provider) bool {
	if s.Endpoint == "" {
		return true
	}
	if !strings.Contains(s.Endpoint, "://") {
		if (route.Route{Provider: s.Provider, Profile: s.Profile, Endpoint: s.Endpoint}).Validate() != nil {
			return false
		}
		if s.Endpoint == EndpointProxy || s.Endpoint == EndpointDirect {
			return true
		}
		cfg, _ := proxyteam.Load(proxy.TeamOverlay())
		team, id, scoped := strings.Cut(s.Endpoint, ":")
		if !slices.ContainsFunc(cfg.Teams, func(t proxyteam.Team) bool { return t.Name == team }) {
			return false
		}
		return !scoped || proxy.ValidScopePart(id)
	}
	return (route.Route{Provider: s.Provider, Profile: s.Profile, Endpoint: s.Endpoint}).Validate() == nil &&
		row != nil && (row.BaseURL == s.Endpoint || slices.Contains(slices.Collect(maps.Values(row.Routes)), s.Endpoint))
}

// modelOK admits a catalog, family or registry model name, with at most one client-only [1m] marker after it.
func (s StoredOverlay) modelOK(reg *providers.Registry) bool {
	name := s.Model
	if models.Has1M(name) {
		name = name[:len(name)-len("[1m]")]
		if name == "" || name == AutoModel || models.Has1M(name) {
			return false
		}
	} else if name == "" || name == AutoModel {
		return true
	}
	if models.IsKnownSpelling(name) || providers.IsFamily(name) {
		return true
	}
	return reg != nil && slices.ContainsFunc(slices.Collect(maps.Values(reg.Providers)), func(p *providers.Provider) bool {
		if p == nil {
			return false
		}
		_, alias := p.Models[name]
		return alias || slices.Contains(slices.Collect(maps.Values(p.Models)), name)
	})
}

// ReplayOverlay reads a record's stored overlay (or old fields) and layers typed flags/--overlay with OriginRecord.
func (r Record) ReplayOverlay(flags []string, typed agentconfig.Overlay) (agentconfig.Overlay, agentconfig.Provenance, error) {
	s := r.fields()
	if r.Overlay != nil {
		s = *r.Overlay
		if err := s.check(); err != nil {
			return agentconfig.Overlay{}, nil, fmt.Errorf("stored overlay refused: %w", err)
		}
	}
	u := r.replayUnit(route.Route{Endpoint: s.Endpoint, Provider: s.Provider, Profile: s.Profile, Identity: s.Identity, Account: s.Account})
	s.Endpoint, s.Provider, s.Profile, s.Identity, s.Account = u.Endpoint, u.Provider, u.Profile, u.Identity, u.Account
	if err := s.check(); err != nil {
		return agentconfig.Overlay{}, nil, fmt.Errorf("stored overlay refused: %w", err)
	}
	base := agentconfig.Overlay{Model: s.Model, Fallback: s.Fallback, MaxChildren: s.MaxChildren, MaxTreeTokens: s.MaxTreeTokens,
		MaxDepth: s.MaxDepth, BudgetUSD: s.BudgetUSD, BudgetCredits: s.BudgetCredits}
	base.Endpoint, base.Provider, base.Profile, base.Identity, base.Account = s.Endpoint, s.Provider, s.Profile, s.Identity, s.Account
	eff, err := DeriveUnit(base, typed, agentconfig.HopResume) // Derive copies: the record's pointers are never shared
	if err != nil {
		return agentconfig.Overlay{}, nil, err
	}
	return eff, agentconfig.RecordOrigins(flags, typed, base, eff), nil
}

// replayUnit prefers the answering hop; a partial observation keeps launch details only on the same recorded row.
func (r Record) replayUnit(stored route.Route) route.Route {
	u := cmp.Or(r.RanRoute, ranRoute(wire.Result{Hops: r.Attempts}), route.ParseLegacy(r.Provider))
	if u.IsZero() {
		u = cmp.Or(stored, r.Request.EffectiveRoute(), r.Route)
	}
	if u.Provider == "" && u.Profile != "" {
		if reg, err := providers.LoadShared(); err == nil {
			if row := reg.Row(u.Profile); row != nil {
				u.Provider = row.Provider
			} else if ValidAgentPin(reg, cmp.Or(r.RanAs, r.Agent), u.Profile) == nil {
				return route.Route{} // a registered native login has no provider unit to inherit from the failed lead
			}
		}
	}
	if u.Provider == stored.Provider && u.Profile == stored.Profile {
		u.Endpoint, u.Identity, u.Account = cmp.Or(u.Endpoint, stored.Endpoint), cmp.Or(u.Identity, stored.Identity), cmp.Or(u.Account, stored.Account)
	} else if stored.Profile == route.AutoArm && u.Provider == stored.Provider && u.Profile != "" {
		if !strings.Contains(stored.Endpoint, "://") { // the auto arm's named endpoint and identity apply to the row it picked
			u.Endpoint = cmp.Or(u.Endpoint, stored.Endpoint)
		}
		u.Identity = cmp.Or(u.Identity, stored.Identity)
	}
	return u
}
