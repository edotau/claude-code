package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	DefaultMaxDispatchChildren = 8
	DefaultMaxTreeTokens       = 0
	maxDispatchChildrenEnv     = "HARNESS_DISPATCH_MAX_CHILDREN"
	maxTreeTokensEnv           = "HARNESS_DISPATCH_MAX_TREE_TOKENS"
)

var dispatchChildrenLockWait = 250 * time.Millisecond

func dispatchLimit(ctx context.Context, key string, fallback int) int {
	if proc.Value(ctx, key) == "" {
		o, _ := agentconfig.LoadOverlayContext(ctx, "", nil)
		return o.Limit(key, fallback)
	}
	n, err := strconv.Atoi(proc.Value(ctx, key))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func claimLimitedRecord(ctx context.Context, name string, rec Record) (Record, error) {
	if rec.Parent == "" {
		return claimRecord(rec), nil
	}
	if max := dispatchLimit(ctx, maxTreeTokensEnv, DefaultMaxTreeTokens); max > 0 {
		root, spent, unknown := dispatchTreeUsage(rec.Parent)
		if spent >= max {
			return Record{}, &retry.Error{Kind: retry.KindFatal, Terminal: true, Agent: name,
				Err: fmt.Errorf("dispatch tree budget %d tokens spent under root %s (%d counted; %d records without usage); set %s (0 = no cap)", max, root, spent, unknown, maxTreeTokensEnv)}
		}
	}
	max := dispatchLimit(ctx, maxDispatchChildrenEnv, DefaultMaxDispatchChildren)
	if max == 0 {
		return claimRecord(rec), nil
	}
	ran := false
	_ = os.MkdirAll(paths.DispatchDir(), 0o700) // the lock would create it 0755; a failure surfaces as the fail-open below
	err := paths.WithFileLockWithin(filepath.Join(paths.DispatchDir(), ".children"),
		dispatchChildrenLockWait, func() error {
			ran = true
			count := liveDispatchChildren(paths.DispatchDir(), rec.Parent)
			if count >= max {
				return &retry.Error{Kind: retry.KindFatal, Terminal: true, Agent: name,
					Err: fmt.Errorf("dispatch width %d reached for parent %s (%d running); wait for one to finish or set %s (0 = no cap)", max, rec.Parent, count, maxDispatchChildrenEnv)}
			}
			rec = claimRecord(rec)
			return nil
		})
	if !ran { // Lock failures must never stall dispatch.
		return claimRecord(rec), nil
	}
	return rec, err
}

// childIndexFile caches each record's admission fields; not *.json, so record listings never see it.
const childIndexFile = ".children.idx"

// childEntry is one record's admission fields, valid while its mtime and size are unchanged.
type childEntry struct {
	Mod    int64     `json:"m"`
	Size   int64     `json:"s"`
	Parent string    `json:"p,omitempty"`
	Status string    `json:"st,omitempty"`
	PID    int       `json:"pid,omitempty"`
	Time   time.Time `json:"t,omitzero"`
}

// liveDispatchChildren counts parent's live children, reopening only records whose mtime or size changed.
func liveDispatchChildren(dir, parent string) int {
	idx := filepath.Join(dir, childIndexFile)
	old := map[string]childEntry{}
	if b, err := os.ReadFile(idx); err == nil {
		_ = json.Unmarshal(b, &old) // a bad index is rebuilt from the records
	}
	names, _ := recordNames(dir) // Unreadable records fail open, like dispatch history.
	next := make(map[string]childEntry, len(names))
	changed := len(old) != len(names)
	count := 0
	for _, n := range names {
		e, ok := childEntryFor(filepath.Join(dir, n), old[n])
		if !ok {
			changed = true
			continue
		}
		changed = changed || e != old[n]
		next[n] = e
		if e.Parent == parent && inflight.DispatchAlive(e.Status, e.PID, e.Time) {
			count++
		}
	}
	if changed {
		if b, err := json.Marshal(next); err == nil {
			_ = paths.AtomicWriteNoSync(idx, b, 0o600) // a cache: a failed write only costs the next scan a reread
		}
	}
	return count
}

// childEntryFor is cached when path's stat matches it, else read afresh; false for an unreadable record.
func childEntryFor(path string, cached childEntry) (childEntry, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return childEntry{}, false
	}
	e := childEntry{Mod: fi.ModTime().UnixNano(), Size: fi.Size()}
	if cached.Mod == e.Mod && cached.Size == e.Size {
		return cached, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return childEntry{}, false
	}
	if rec, derr := decodeRecord(b); derr == nil && rec.Parent != "" && rec.Status == "running" {
		e.Parent, e.Status, e.PID, e.Time = rec.Parent, rec.Status, rec.PID, rec.Time
	}
	return e, true
}

func dispatchTreeUsage(parent string) (string, int, int) {
	root := parent
	seen := map[string]bool{}
	for !seen[root] {
		seen[root] = true
		rec, err := LoadRecord(root)
		if err != nil || rec.Parent == "" {
			break
		}
		root = rec.Parent
	}
	records := dispatchTreeRecords(root)
	members := map[string]bool{root: true}
	spent, unknown := 0, 0
	for _, rec := range slices.Backward(records) {
		if !members[rec.ID] && !members[rec.Parent] {
			continue
		}
		members[rec.ID] = true
		if rec.Usage == nil {
			unknown++
			continue
		}
		u := rec.Usage
		spent += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
	}
	return root, spent, unknown
}

func dispatchTreeRecords(root string) []Record {
	var records []Record
	_ = eachRecent(paths.DispatchDir(), "", func(rec Record) bool { // Missing history cannot supply usage.
		records = append(records, rec)
		return rec.ID != root
	})
	return records
}
