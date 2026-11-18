package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
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

func liveDispatchChildren(dir, parent string) int {
	count := 0
	_ = eachRecent(dir, jsonToken(parent), func(rec Record) bool { // Unreadable records fail open, like dispatch history.
		if rec.Parent == parent && rec.Alive() {
			count++
		}
		return true
	})
	return count
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
