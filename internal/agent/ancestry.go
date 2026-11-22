package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
)

// ancestryOff keeps tests off the live process tree, which may sit under a real dispatch.
var ancestryOff = testing.Testing()

// ancestorDispatch is the nearest running dispatch above this process, looked up once per process.
var ancestorDispatch = sync.OnceValues(findAncestorDispatch)

// withAncestorDispatch fills a dispatch env a leaf wiped from the nearest running dispatch above it; a set id wins.
func withAncestorDispatch(values proc.Values) proc.Values {
	if values[proc.DispatchIDEnv] != "" {
		return values
	}
	rec, ok := ancestorDispatch()
	if !ok {
		return values
	}
	set := proc.Values{proc.DispatchIDEnv: rec.ID, proc.DispatchDepthEnv: strconv.Itoa(rec.Depth)}
	p, err := inheritPermissionsContext(proc.WithValues(context.Background(), values), chainPolicy(rec))
	if err == nil && !p.IsZero() { // leave a malformed inherited policy intact so checkRun refuses it
		raw, _ := json.Marshal(p) // strings and string slices always marshal
		set[DispatchPermissionsEnv] = string(raw)
	}
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
	return values.Merge(set)
}

// findAncestorDispatch recovers the nearest live dispatch, combining restrictions when a batch shares its process.
func findAncestorDispatch() (Record, bool) {
	if ancestryOff {
		return Record{}, false
	}
	chain := dispatchAncestors()
	if len(chain) == 0 { // top level: no ancestor runs the harness binary, so no record is read
		return Record{}, false
	}
	recs, _ := ReadRecentWith(AllRecords, inflight.RunningToken) // newest first
	best, ok := nearestDispatch(recs, chain)
	if ok {
		fmt.Fprintf(NoticeOut, "claude-code: no %s in the env; parent dispatch %s found by process ancestry\n", proc.DispatchIDEnv, best.ID)
	}
	return best, ok
}

func nearestDispatch(recs []Record, chain map[int]int) (Record, bool) {
	best, nearest := Record{}, -1
	for _, r := range recs {
		n, ok := chain[r.PID]
		if !ok || r.Status != "running" || (nearest >= 0 && n > nearest) || !liveAs(r.PID, r.PIDStart) {
			continue
		}
		if nearest == n {
			best = restrictDispatch(best, r)
		} else {
			best, nearest = r, n
		}
	}
	return best, nearest >= 0
}

// restrictDispatch keeps a deepest non-carrier identity for accounting, never a sibling's weaker launch restrictions.
func restrictDispatch(a, b Record) Record {
	p, q := chainPolicy(a), chainPolicy(b)
	readOnly := a.Request.ReadOnly || b.Request.ReadOnly
	p.Deny, p.Ask = permission.Union(p.Deny, q.Deny), permission.Union(p.Ask, q.Ask)
	p.Allow = slices.DeleteFunc(slices.Clone(p.Allow), func(r permission.Rule) bool { return !slices.Contains(q.Allow, r) })
	if readOnly {
		p.Deny = permission.Union(p.Deny, permission.ReadOnlyDeny)
	}
	if p.Mode == permission.PermPlan || q.Mode == permission.PermPlan {
		p.Mode = permission.PermPlan
	} else if p.Mode != q.Mode {
		p.Mode = permission.PermDefault
	}
	depth := minDispatchCap(a.MaxDepth, b.MaxDepth, DefaultMaxDispatchDepth)
	children := minDispatchCap(a.MaxChildren, b.MaxChildren, DefaultMaxDispatchChildren)
	tokens := minDispatchCap(a.MaxTreeTokens, b.MaxTreeTokens, DefaultMaxTreeTokens)
	if b.Depth > a.Depth || b.Depth == a.Depth && a.Request.Carry && !b.Request.Carry {
		a, b = b, a
	}
	a.ExportedPolicy, a.MaxDepth, a.MaxChildren, a.MaxTreeTokens = &p, &depth, &children, &tokens
	a.Request.ReadOnly = readOnly
	return a
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
