package router

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// A refresh that records nothing stamps the account checked: the next pick inside StaleAfter skips the fetch and the
// account still reads unknown; past the TTL the pick fetches again, each account once.
func TestPickBacksOffAfterNoDataRefresh(t *testing.T) {
	tb := testTable(t)
	at := now0
	var mu sync.Mutex
	var calls []string
	o := PickOpts{Fallback: FallbackAgents, Configured: allConfigured, Now: func() time.Time { return at },
		WorkRepo: func() bool { return false },
		Refresh: func(_ context.Context, account string) bool {
			mu.Lock()
			calls = append(calls, account)
			mu.Unlock()
			if err := MarkChecked(account, "unlimited", at); err != nil {
				t.Error(err)
			}
			return false
		}}
	pick := func() []string {
		calls = nil
		tb.Pick(context.Background(), "sonnet", "", LoadState(), o)
		slices.Sort(calls)
		return calls
	}
	first := pick()
	if len(first) == 0 || len(slices.Compact(slices.Clone(first))) != len(first) {
		t.Fatalf("first pick refreshed %v, want each stale account exactly once", first)
	}
	if again := pick(); len(again) != 0 {
		t.Errorf("pick inside StaleAfter refreshed %v, want none (checked stamp)", again)
	}
	for _, account := range first {
		if e := LoadState()[account]; e.Status(at) != StateUnknown || e.CheckNote != "unlimited" {
			t.Errorf("%s = %+v, want unknown with the check note", account, e)
		}
	}
	at = at.Add(StaleAfter + time.Minute)
	if after := pick(); !slices.Equal(after, first) {
		t.Errorf("pick past StaleAfter refreshed %v, want %v", after, first)
	}
}

// A refresh that writes nothing (lock busy, caller's deadline) still runs once per account per pick, never again serially.
func TestPickRefreshesEachAccountOnce(t *testing.T) {
	tb := testTable(t)
	var mu sync.Mutex
	seen := map[string]int{}
	tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAgents, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false },
		Refresh: func(_ context.Context, account string) bool {
			mu.Lock()
			seen[account]++
			mu.Unlock()
			return false
		}})
	for account, n := range seen {
		if n != 1 {
			t.Errorf("%s refreshed %d times in one pick, want 1", account, n)
		}
	}
	if len(seen) == 0 {
		t.Error("no account refreshed")
	}
}

// The collecting pass must not advance the Discovery rotation; the real pick keeps the pin.
func TestDryPickDropsDiscoveryPin(t *testing.T) {
	if got := (PickOpts{dry: true}).modelPin(DiscoveryModel); got != "" {
		t.Errorf("dry modelPin(discovery) = %q, want \"\"", got)
	}
	if got := (PickOpts{}).modelPin(DiscoveryModel); got != DiscoveryModel {
		t.Errorf("real modelPin(discovery) = %q, want the pin", got)
	}
}
