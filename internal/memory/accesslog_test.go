package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// One spanning pass over the access fold: 30-day rotation keeps the cold horizon, ReturnCounts credits
// only explicit search/show returns, and nominations cover a non-home repo's log under its store key.
func TestAccessRotationReturnCountsAndRepoNominations(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	home := HomeBankDir()

	old := now.AddDate(0, 0, -(accessRotateDays + 1))
	LogAccess(home, old, "aa000001", AccessRecall)
	LogAccess(home, now, "bb000002", AccessRecall) // the write that must rotate
	logPath := AccessLogPath(home)
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("a first entry older than %dd did not rotate to .1: %v", accessRotateDays, err)
	}
	stats, oldest := AccessStats(home, now, 0)
	if !oldest.Equal(old) {
		t.Errorf("rotation lost the cold horizon: oldest = %v, want %v", oldest, old)
	}
	if s := stats["aa000001"]; s.Count != 1 {
		t.Errorf("the rotated entry must still count: %+v", s)
	}

	// ReturnCounts: search/show returns inside the window count; recall injections and old lines never do.
	LogAccess(home, now.AddDate(0, 0, -10), "cc000003", AccessSearch)
	LogAccess(home, now.AddDate(0, 0, -5), "cc000003", AccessShow)
	LogAccess(home, now.AddDate(0, 0, -2), "cc000003", AccessRecall)
	LogAccess(home, now.AddDate(0, 0, -(accessRotateDays+5)), "cc000003", AccessSearch)
	counts := ReturnCounts([]string{home}, now)
	if counts["cc000003"] != 2 {
		t.Errorf("ReturnCounts[cc000003] = %d, want 2 (in-window search + show)", counts["cc000003"])
	}
	if counts["aa000001"] != 0 || counts["bb000002"] != 0 {
		t.Errorf("recall injections must never count as returns: %v", counts)
	}

	// Nominations glob every repo's state log, prefixed with the repo's store key — not only home's.
	root := repoRoot(t, "proj")
	dir := Dir(root)
	for d := range hotMinDays {
		LogAccess(dir, now.AddDate(0, 0, -d), "dd000004", AccessRecall)
	}
	want := filepath.Base(dir) + "/dd000004: hot"
	noms := AccessNominations(now)
	found := false
	for _, n := range noms {
		if strings.HasPrefix(n, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("want a hot nomination %q, got %v", want, noms)
	}
}

// Regression: concurrent writers that all see a stale first line rotate once, and no line is lost.
func TestAccessLogConcurrentRotationRotatesOnce(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	home := HomeBankDir()
	LogAccess(home, now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", AccessRecall)
	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { LogAccess(home, now, fmt.Sprintf("bb%06d", i), AccessSearch) })
	}
	wg.Wait()
	path := AccessLogPath(home)
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("concurrent writers rotated more than once")
	}
	if n := len(ReturnCounts([]string{home}, now)); n != writers {
		t.Fatalf("%d of %d concurrent lines survived", n, writers)
	}
}
