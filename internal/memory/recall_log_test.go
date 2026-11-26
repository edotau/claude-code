package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// Rotation preserves history; returns exclude old reads and injections.
func TestAccessRotationReturnCountsAndNominations(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -(accessRotateDays + 1))
	logAccess(old, "aa000001", accessRecall)
	logAccess(old, "cc000003", AccessSearch)
	logAccess(now, "bb000002", accessRecall) // the write that must rotate
	logPath := accessLogPath()
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("a first entry older than %dd did not rotate to .1: %v", accessRotateDays, err)
	}
	stats, oldest := accessStatsAt(accessLogPath(), now, 0)
	if !oldest.Equal(old) {
		t.Errorf("rotation lost the cold horizon: oldest = %v, want %v", oldest, old)
	}
	if s := stats["aa000001"]; s.Newest.IsZero() {
		t.Errorf("the rotated entry must still count: %+v", s)
	}
	// returnCounts: search/show returns inside the window count; recall injections and old lines never do.
	logAccess(now.AddDate(0, 0, -10), "cc000003", AccessSearch)
	logAccess(now.AddDate(0, 0, -5), "cc000003", AccessShow)
	logAccess(now.AddDate(0, 0, -2), "cc000003", accessRecall)
	counts := returnCounts(now)
	if counts["cc000003"] != 2 {
		t.Errorf("returnCounts[cc000003] = %d, want 2 (in-window search + show)", counts["cc000003"])
	}
	if counts["aa000001"] != 0 || counts["bb000002"] != 0 {
		t.Errorf("recall injections must never count as returns: %v", counts)
	}

	assertHotNomination(t, now)
}

// Nominations prefix entries with the home bank's key.
func assertHotNomination(t *testing.T, now time.Time) {
	t.Helper()
	for d := range hotMinDays {
		logAccess(now.AddDate(0, 0, -d), "dd000004", accessRecall)
	}
	want := "home/dd000004: hot"
	if !slices.ContainsFunc(accessNominations(now), func(n string) bool { return strings.HasPrefix(n, want) }) {
		t.Errorf("want a hot nomination %q, got %v", want, accessNominations(now))
	}
}

// Regression: concurrent writers that all see a stale first line rotate once, and no line is lost.
func TestAccessLogConcurrentRotationRotatesOnce(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	logAccess(now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", accessRecall)
	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { logAccess(now, fmt.Sprintf("bb%06d", i), AccessSearch) })
	}
	wg.Wait()
	path := accessLogPath()
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("concurrent writers rotated more than once")
	}
	if n := len(returnCounts(now)); n != writers {
		t.Fatalf("%d of %d concurrent lines survived", n, writers)
	}
}

// Every hit, wherever its file sits, is credited to the one bank's access log.
func TestLogHitsCreditsTheOneBank(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	write(t, paths.MemorySub("archive", "old.md"), "# A\n\n## 2026-01-01 — zebra archived quota <!-- id:bbbb0001 -->\n\nzebra quota\n")
	write(t, Path("decisionLog.md"), "# D\n\n## 2026-01-01 — zebra live quota <!-- id:bbbb0003 -->\n\nzebra quota\n")
	now := time.Now()
	logHits(SearchBank("zebra quota", 0), AccessSearch, now, "")
	got := returnCounts(now)
	for _, id := range []string{"bbbb0001", "bbbb0003"} {
		if got[id] == 0 {
			t.Errorf("%s not credited to the one bank: %v", id, got)
		}
	}
}

func TestLogAccessAndAccessStatsAt(t *testing.T) {
	tempConfig(t)
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	logAccess(day1, "aa11bb22", accessRecall)
	logAccess(day1.AddDate(0, 0, 1), "aa11bb22", accessRecall)
	logAccess(day1.AddDate(0, 0, 2), "aa11bb22", accessRecall)
	logAccess(day1, "cc33dd44", accessRecall)

	stats, oldest := accessStatsAt(accessLogPath(), day1.AddDate(0, 0, 2), 30)
	if oldest.IsZero() || !oldest.Equal(day1) {
		t.Errorf("oldest = %v want %v", oldest, day1)
	}
	if s := stats["aa11bb22"]; s.Days != 3 {
		t.Errorf("decisionLog stat %+v", s)
	}
	if s := stats["cc33dd44"]; s.Days != 1 {
		t.Errorf("conventions stat %+v", s)
	}
}

func TestAccessNominationsHotAndCold(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for d := range 5 {
		logAccess(now.AddDate(0, 0, -d), "aa11bb22", accessRecall)
	}
	// Log must cover > coldDays for a cold nomination to fire at all.
	logAccess(now.AddDate(0, 0, -(coldDays+5)), "ee55ff66", accessRecall)

	noms := accessNominations(now)
	joined := strings.Join(noms, "\n")
	if !strings.Contains(joined, "home/aa11bb22: hot") {
		t.Errorf("want a hot nomination, got %v", noms)
	}
	if !strings.Contains(joined, "home/ee55ff66: not recalled") {
		t.Errorf("want a cold nomination, got %v", noms)
	}
}

// A repeat same-day inject adds no line; a new day, a new entry or an explicit search still does.
func TestLogAccessRecallInjectOncePerEntryPerDay(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	logAccessBatch(now, []string{"aa000001", "aa000001", "bb000002"}, accessRecall, "")
	logAccess(now.Add(time.Hour), "aa000001", accessRecall)
	logAccess(now.Add(time.Hour), "aa000001", AccessSearch)
	logAccess(now.AddDate(0, 0, 1), "aa000001", accessRecall)
	data, _ := os.ReadFile(accessLogPath())
	if got := strings.Count(string(data), "\n"); got != 4 {
		t.Errorf("want 4 lines (aa+bb injects day 1, aa search, aa inject day 2), got %d:\n%s", got, data)
	}
}

// A busy day of repeat injects no longer evicts history: hot fires after 3 days, cold after a 90d-old log.
func TestAccessNominationsSurviveBusyDays(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	logAccess(now.AddDate(0, 0, -(coldDays+5)), "ee55ff66", accessRecall)
	for d := range hotMinDays {
		for range 50 {
			logAccess(now.AddDate(0, 0, -d), "aa11bb22", accessRecall)
		}
	}
	joined := strings.Join(accessNominations(now), "\n")
	if !strings.Contains(joined, "home/aa11bb22: hot") || !strings.Contains(joined, "home/ee55ff66: not recalled") {
		t.Errorf("want hot and cold nominations, got %q", joined)
	}
}

// An unopenable lock file degrades to a plain append: no rotate runs unlocked.
func TestLogAccessLockUnopenableAppendsOnly(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	path := accessLogPath()
	logAccess(now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", accessRecall)
	_ = os.Remove(path + ".lock")
	if err := os.Mkdir(path+".lock", 0o700); err != nil { // a directory cannot be opened as the lock file
		t.Fatal(err)
	}
	logAccess(now, "bb000002", accessRecall)
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("rotation ran without the lock")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "aa000001") || !strings.Contains(string(data), "bb000002") {
		t.Errorf("fallback must append to the existing log, got %q", data)
	}
}

// Main-format lines parse as before, plain readers skip session records, and an old log still counts.
func TestAccessLineShapesAndOldLogCounted(t *testing.T) {
	tempConfig(t)
	const sid = "11111111-1111-1111-1111-111111111111"
	for _, c := range []struct{ line, entry, reason, session string }{
		{"2026-11-10T10:00:00Z -- aa000001 -- memory show", "aa000001", "memory show", ""},
		{"2026-11-10T10:00:00Z -- aa000001 -- odd -- tail", "aa000001", "odd -- tail", ""},
		{"2026-11-10T10:00:00Z -- aa000001", "aa000001", "", ""},
		{"session -- " + sid + " -- 2026-11-10T10:00:00Z -- aa000001 -- memory show", "aa000001", "memory show", sid},
		{"garbage", "", "", ""},
	} {
		ts, entry, reason, session := parseAccessLine(c.line)
		if entry != c.entry || reason != c.reason || session != c.session || (c.entry != "" && ts.IsZero()) {
			t.Errorf("%q = %v %q %q %q", c.line, ts, entry, reason, session)
		}
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	path := accessLogPath()
	write(t, path, "2026-11-10T10:00:00Z -- aa000001 -- prompt-recall inject\n2026-11-11T10:00:00Z -- aa000001 -- memory show\n")
	logAccessBatch(now, []string{"aa000001"}, AccessShow, sid)
	stats, _ := accessStatsAt(path, now, 30)
	if got := returnCounts(now)["aa000001"]; stats["aa000001"].Days != 3 || got != 2 {
		t.Errorf("days = %d, returns = %d, want 3 and 2 (the session record must not count twice)", stats["aa000001"].Days, got)
	}
}

// A session's repeat inject of one id writes one session record a day; the plain lines stay one per id and day.
func TestAccessSessionRecordOncePerSessionIDDay(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for range 5 {
		logAccessBatch(now, []string{"aa000001", "bb000002"}, accessRecall, "11111111-1111-1111-1111-111111111111")
	}
	logAccessBatch(now, []string{"aa000001"}, accessRecall, "22222222-2222-2222-2222-222222222222")
	data, _ := os.ReadFile(accessLogPath())
	if got, want := strings.Count(string(data), "\n"), 2+3; got != want {
		t.Errorf("lines = %d, want %d (2 plain + 3 session records):\n%s", got, want, data)
	}
}

// One dispatch's block may reach LogBriefRecall from both the hook and ask; it still credits each id once.
func TestLogBriefRecallTwiceCreditsOnce(t *testing.T) {
	tempConfig(t)
	const block = "\n\n<memory-recall>\n- aa000001 one\n- bb000002 two\n</memory-recall>"
	for range 2 {
		LogBriefRecall("11111111-1111-1111-1111-111111111111", block)
	}
	data, _ := os.ReadFile(accessLogPath())
	if got := strings.Count(string(data), "session -- "); got != 2 {
		t.Errorf("session records = %d, want 2:\n%s", got, data)
	}
}

func logAccess(now time.Time, entry, reason string) {
	logAccessBatch(now, []string{entry}, reason, "")
}

// Access traces always live in state, never in the store.
func TestAccessLogPathOutsideStore(t *testing.T) {
	tempConfig(t)
	if got, want := accessLogPath(), filepath.Join(AccessStateDir(), "home.log"); got != want {
		t.Fatalf("accessLogPath() = %s, want %s", got, want)
	}
	logAccess(time.Now(), "aa11bb22", "read")
	if _, err := os.Stat(filepath.Join(paths.MemoryStoreDir(), "access.log")); !os.IsNotExist(err) {
		t.Errorf("access.log written inside the store: %v", err)
	}
	if data, _ := os.ReadFile(accessLogPath()); !strings.Contains(string(data), "aa11bb22") {
		t.Errorf("state access log missing the entry: %q", data)
	}
}

func TestLogAccessRedactsCredentialShapedReason(t *testing.T) {
	tempConfig(t)
	logAccess(time.Now(), "aa11bb22", "leaked sk-abcdefghijklmnopqrstuvwx in reason")
	data := mustRead(t, accessLogPath())
	if strings.Contains(data, "sk-abcdefghijklmnopqrstuvwx") {
		t.Error("a credential-shaped reason must be redacted, not logged verbatim")
	}
	if !strings.Contains(data, "redacted") {
		t.Errorf("log %q", data)
	}
}

func TestLearnRecall(t *testing.T) {
	tempConfig(t)
	now := time.Now().UTC()
	const elsewhere = "22222222-2222-2222-2222-222222222222"
	write(t, Path("conventions.md"), `# Conventions

- Used <!-- id:aaaa0001 -->
- Unused <!-- id:aaaa0002 -->
`)
	sessions := make([]string, recallMinInjections)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("11111111-1111-1111-1111-%012d", i)
		logAccessBatch(now.Add(-time.Minute), []string{"aaaa0001", "aaaa0002"}, accessRecall, sessions[i])
	}
	logAccessBatch(now.Add(-time.Minute), []string{"aaaa0003"}, accessRecall, sessions[0])
	for _, s := range sessions {
		t.Setenv(paths.SessionIDEnv, s)
		showRecallFixture(t, "aaaa0001")
	}
	t.Setenv(paths.SessionIDEnv, elsewhere)
	showRecallFixture(t, "aaaa0002")
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"aaaa0001": 1.2, "aaaa0002": 0.8, "aaaa0003": 1} {
		if got := boostFactor(recallWeights(), id); got != want {
			t.Errorf("%s factor = %v, want %v", id, got, want)
		}
	}
	assertRecallNoRewrite(t, now)
}

func showRecallFixture(t *testing.T, id string) {
	t.Helper()
	if _, _, err := Show(id); err != nil {
		t.Fatal(err)
	}
}

func assertRecallNoRewrite(t *testing.T, now time.Time) {
	t.Helper()
	path := recallLearningPath()
	stamp := now.Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatal("unchanged access log caused the learner to rewrite state")
	}
}

func BenchmarkRecallRanking(b *testing.B) {
	b.Cleanup(paths.SetStampNowForTest(func() time.Time { return time.Now().Add(time.Hour) }))
	for _, present := range []bool{false, true} {
		b.Run(fmt.Sprintf("weights=%t", present), func(b *testing.B) {
			b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
			b.Setenv("HOME", b.TempDir())
			b.Setenv(UpdateChildEnv, "")
			var bank strings.Builder
			bank.WriteString("# Conventions\n\n")
			for i := range 5 {
				fmt.Fprintf(&bank, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
			}
			write(b, Path("conventions.md"), bank.String())
			logAccess(time.Now(), "00000001", AccessShow)
			if present {
				path := filepath.Join(paths.MemoryStateDir(), "recall-learning.json")
				write(b, path, `{"days":{"2026-11-18":{"00000001":{"injected":5,"used":5}}}}`)
			}
			recallPicks(context.Background(), "", recallTask, false, false)
			for b.Loop() {
				recallPicks(context.Background(), "", recallTask, false, false)
			}
		})
	}
}
