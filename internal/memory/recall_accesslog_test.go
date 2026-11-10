package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Rotation preserves history; returns exclude old reads and injections; nominations include other repos.
func TestAccessRotationReturnCountsAndRepoNominations(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	home := HomeBankDir()
	old := now.AddDate(0, 0, -(accessRotateDays + 1))
	logAccess(home, old, "aa000001", AccessRecall)
	logAccess(home, old, "cc000003", AccessSearch)
	logAccess(home, now, "bb000002", AccessRecall) // the write that must rotate
	logPath := AccessLogPath(home)
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("a first entry older than %dd did not rotate to .1: %v", accessRotateDays, err)
	}
	stats, oldest := accessStatsAt(AccessLogPath(home), now, 0)
	if !oldest.Equal(old) {
		t.Errorf("rotation lost the cold horizon: oldest = %v, want %v", oldest, old)
	}
	if s := stats["aa000001"]; s.Count != 1 {
		t.Errorf("the rotated entry must still count: %+v", s)
	}
	// returnCounts: search/show returns inside the window count; recall injections and old lines never do.
	logAccess(home, now.AddDate(0, 0, -10), "cc000003", AccessSearch)
	logAccess(home, now.AddDate(0, 0, -5), "cc000003", AccessShow)
	logAccess(home, now.AddDate(0, 0, -2), "cc000003", AccessRecall)
	counts := returnCounts([]string{home}, now)
	if counts["cc000003"] != 2 {
		t.Errorf("returnCounts[cc000003] = %d, want 2 (in-window search + show)", counts["cc000003"])
	}
	if counts["aa000001"] != 0 || counts["bb000002"] != 0 {
		t.Errorf("recall injections must never count as returns: %v", counts)
	}

	assertHotNomination(t, now)
}

// Nominations glob every repo's state log, prefixed with the repo's store key — not only home's.
func assertHotNomination(t *testing.T, now time.Time) {
	t.Helper()
	root := repoRoot(t, "proj")
	dir := Dir(root)
	for d := range hotMinDays {
		logAccess(dir, now.AddDate(0, 0, -d), "dd000004", AccessRecall)
	}
	want := filepath.Base(dir) + "/dd000004: hot"
	if !slices.ContainsFunc(accessNominations(now), func(n string) bool { return strings.HasPrefix(n, want) }) {
		t.Errorf("want a hot nomination %q, got %v", want, accessNominations(now))
	}
}

// Regression: concurrent writers that all see a stale first line rotate once, and no line is lost.
func TestAccessLogConcurrentRotationRotatesOnce(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	home := HomeBankDir()
	logAccess(home, now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", AccessRecall)
	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { logAccess(home, now, fmt.Sprintf("bb%06d", i), AccessSearch) })
	}
	wg.Wait()
	path := AccessLogPath(home)
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("concurrent writers rotated more than once")
	}
	if n := len(returnCounts([]string{home}, now)); n != writers {
		t.Fatalf("%d of %d concurrent lines survived", n, writers)
	}
}

// Every hit is credited to the bank dir that owns it: repo native/archive, and --all's local corpus.
func TestLogHitsCreditsOwningStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(da, "archive", "old.md"), "# A\n\n## 2026-01-01 — zebra archived quota <!-- id:bbbb0001 -->\n\nzebra quota\n")
	write(t, filepath.Join(da, "notes.md"), "---\nname: n\n---\nzebra native quota <!-- id:bbbb0002 -->\n")
	harness := Dir(paths.ConfigDir())
	write(t, filepath.Join(harness, "decisionLog.md"), "# D\n\n## 2026-01-01 — zebra local quota <!-- id:bbbb0003 -->\n\nzebra quota\n")

	now := time.Now()
	logHits(a, SearchBanks(a, "zebra quota", 0), AccessSearch, now)
	got := returnCounts([]string{da}, now)
	home := returnCounts([]string{HomeBankDir()}, now)
	for _, id := range []string{"bbbb0001", "bbbb0002"} {
		if got[id] == 0 || home[id] != 0 {
			t.Errorf("%s must be credited to the repo store, not home: repo=%d home=%d", id, got[id], home[id])
		}
	}

	logHits(a, searchAll("zebra quota", 0), AccessSearch, now)
	if c := returnCounts([]string{harness}, now); c["bbbb0003"] == 0 {
		t.Errorf("--all's local hit must credit the harness store: %v", c)
	}
	if c := returnCounts([]string{da}, now); c["bbbb0003"] != 0 {
		t.Errorf("--all's local hit was credited to the caller's repo: %v", c)
	}
}

func TestLogAccessAndAccessStatsAt(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	logAccess(dir, day1, "aa11bb22", AccessRecall)
	logAccess(dir, day1.AddDate(0, 0, 1), "aa11bb22", AccessRecall)
	logAccess(dir, day1.AddDate(0, 0, 2), "aa11bb22", AccessRecall)
	logAccess(dir, day1, "cc33dd44", AccessRecall)

	stats, oldest := accessStatsAt(AccessLogPath(dir), day1.AddDate(0, 0, 2), 30)
	if oldest.IsZero() || !oldest.Equal(day1) {
		t.Errorf("oldest = %v want %v", oldest, day1)
	}
	if s := stats["aa11bb22"]; s.Count != 3 || s.Days != 3 {
		t.Errorf("decisionLog stat %+v", s)
	}
	if s := stats["cc33dd44"]; s.Count != 1 || s.Days != 1 {
		t.Errorf("conventions stat %+v", s)
	}
}

func TestAccessNominationsHotAndCold(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	dir := HomeBankDir()
	for d := range 5 {
		logAccess(dir, now.AddDate(0, 0, -d), "aa11bb22", AccessRecall)
	}
	// Log must cover > coldDays for a cold nomination to fire at all.
	logAccess(dir, now.AddDate(0, 0, -(coldDays+5)), "ee55ff66", AccessRecall)

	noms := accessNominations(now)
	joined := strings.Join(noms, "\n")
	if !strings.Contains(joined, "home/aa11bb22: hot") {
		t.Errorf("want a hot nomination, got %v", noms)
	}
	if !strings.Contains(joined, "home/ee55ff66: not recalled") {
		t.Errorf("want a cold nomination, got %v", noms)
	}
}

// An unopenable lock file degrades to a plain append: no rotate or trim runs unlocked.
func TestLogAccessLockUnopenableAppendsOnly(t *testing.T) {
	tempConfig(t)
	dir := t.TempDir()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	path := AccessLogPath(dir)
	logAccess(dir, now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", AccessRecall)
	_ = os.Remove(path + ".lock")
	if err := os.Mkdir(path+".lock", 0o700); err != nil { // a directory cannot be opened as the lock file
		t.Fatal(err)
	}
	logAccess(dir, now, "bb000002", AccessRecall)
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("rotation ran without the lock")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "aa000001") || !strings.Contains(string(data), "bb000002") {
		t.Errorf("fallback must append to the existing log, got %q", data)
	}
}
