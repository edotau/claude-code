package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
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
	if doc := agentconfig.InlineOverlay(rec.Route.Provider, rec.Route.Profile); doc != "" { // the parent's fence beats any ambient one
		set[route.OverlayVar] = doc
	}
	p, err := InheritPermissions(proc.WithValues(context.Background(), values), chainPolicy(rec))
	if err == nil && !p.IsZero() { // leave a malformed inherited policy intact so checkRun refuses it
		raw, _ := json.Marshal(p) // strings and string slices always marshal
		set[DispatchPermissionsEnv] = string(raw)
	}
	return values.Merge(set).Merge(inheritedCaps(values, rec))
}

// inheritedCaps is rec's depth, children and tree-token caps, each tightened by a valid value already in values.
func inheritedCaps(values proc.Values, rec Record) proc.Values {
	set := proc.Values{}
	for key, n := range map[string]*int{
		maxDispatchDepthEnv: rec.MaxDepth, maxDispatchChildrenEnv: rec.MaxChildren, maxTreeTokensEnv: rec.MaxTreeTokens,
	} {
		if n != nil {
			limit := *n
			if existing, err := strconv.Atoi(values[key]); err == nil && existing >= 0 {
				limit = minDispatchCap(n, &existing, limit)
			}
			set[key] = strconv.Itoa(limit)
		}
	}
	return set
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
		e, rec, ok := pidEntry(filepath.Join(dir, n), old[n], chain)
		if !ok {
			continue
		}
		changed = changed || e != old[n]
		next[n] = e
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
	for n, e := range next {
		c, ok := cur[n]
		if c == e || ok && (c.Mod != e.Mod || c.Size != e.Size) && !statIs(filepath.Join(dir, n), e) {
			continue
		}
		cur[n], changed = e, true
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

// chainPolicy prefers the complete launch snapshot; legacy levels reconstruct defaults and requests until a snapshot.
func chainPolicy(rec Record) permission.Permissions {
	var p permission.Permissions
	seen := map[string]bool{}
	for ok := true; ok && !seen[rec.ID] && len(seen) < maxAncestorWalk; rec, ok = parentRecord(rec.Parent) {
		seen[rec.ID] = true
		if rec.ExportedPolicy != nil {
			return layerOuter(p, *rec.ExportedPolicy)
		}
		own := rec.Request.Permissions
		if def, err := DefaultPermissions(rec.Agent); err == nil { // an unreadable default keeps the request's own rules
			own, _ = permission.Resolve(def, own)
		}
		p = layerOuter(p, own)
	}
	return p
}
