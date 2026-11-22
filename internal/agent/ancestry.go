package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if values[DispatchPermissionsEnv] == "" {
		if p := chainPolicy(rec); !p.IsZero() {
			raw, _ := json.Marshal(p) // strings and string slices always marshal
			set[DispatchPermissionsEnv] = string(raw)
		}
	}
	for key, n := range map[string]*int{maxDispatchChildrenEnv: rec.MaxChildren, maxTreeTokensEnv: rec.MaxTreeTokens} {
		if n != nil && values[key] == "" {
			set[key] = strconv.Itoa(*n)
		}
	}
	return values.Merge(set)
}

// findAncestorDispatch is the running record whose pid and start time match the nearest live ancestor; newest on a tie.
func findAncestorDispatch() (Record, bool) {
	if ancestryOff {
		return Record{}, false
	}
	chain := dispatchAncestors()
	if len(chain) == 0 { // top level: no ancestor runs the harness binary, so no record is read
		return Record{}, false
	}
	recs, _ := ReadRecentWith(AllRecords, inflight.RunningToken) // newest first
	best, nearest := Record{}, -1
	for _, r := range recs {
		if n, ok := chain[r.PID]; ok && r.Status == "running" && (nearest < 0 || n < nearest) && liveAs(r.PID, r.PIDStart) {
			best, nearest = r, n
		}
	}
	if nearest < 0 {
		return Record{}, false
	}
	fmt.Fprintf(NoticeOut, "claude-code: no %s in the env; parent dispatch %s found by process ancestry\n", proc.DispatchIDEnv, best.ID)
	return best, true
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

// chainPolicy is the policy rec exported to its children: each recorded level's defaults and request, layered.
func chainPolicy(rec Record) permission.Permissions {
	var p permission.Permissions
	seen := map[string]bool{}
	for ok := true; ok && !seen[rec.ID] && len(seen) < maxAncestorWalk; rec, ok = parentRecord(rec.Parent) {
		seen[rec.ID] = true
		own := rec.Request.Permissions
		if def, err := DefaultPermissions(rec.Agent); err == nil { // an unreadable default keeps the request's own rules
			own, _ = permission.Resolve(def, own)
		}
		p = layerOuter(p, own)
	}
	return p
}
