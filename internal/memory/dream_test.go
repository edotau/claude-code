package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// A failed run's retry deadline is independent of changes to either bank.
func TestPersistenceFailedDreamRetriesWithOldBanks(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("agent unavailable")
	run := func(context.Context, string, string) (string, error) { return "stub", failure }
	if err := dream(dreamOptions{CWD: root, Run: run}); !errors.Is(err, failure) {
		t.Fatalf("Dream = %v, want agent failure", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for name := range templates() {
		path := Path(name)
		write(t, path, templates()[name])
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := newestMtime(); got.After(old) {
		t.Fatalf("bank fixture has recent mtime: %v", got)
	}
	if dreamDue(now.Add(dreamRetry - time.Minute)) {
		t.Error("failed dream is due before six-hour backoff")
	}
	if !dreamDue(now.Add(dreamRetry + time.Minute)) {
		t.Error("failed dream with two-day-old banks is not due after six hours")
	}
}

// TestPreviewDreamReadsEachBankFileOnce: the cap, archive and rotation previews share one read per typed file.
func TestPreviewDreamReadsEachBankFileOnce(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	var conv, hist strings.Builder
	conv.WriteString("# Conventions\n")
	for i := range 90 {
		fmt.Fprintf(&conv, "\n- Feedback: rule %d", i)
	}
	hist.WriteString("# Session History\n")
	for i := range sessionHistoryMaxEntries + 5 {
		fmt.Fprintf(&hist, "\n## 2026-10-01 — session %d\n\nbody\n", i)
	}
	write(t, Path("conventions.md"), conv.String()+"\n")
	write(t, Path("sessionHistory.md"), hist.String())
	_, _, overflow, err := splitOverflow(Path("conventions.md"), archiveCaps["conventions.md"])
	if err != nil || len(overflow) == 0 {
		t.Fatalf("fixture has no conventions overflow: %v", err)
	}
	reads := countBankReads(t)
	p := previewDream()
	if p.Archivable != len(overflow) || p.SessionRotate != 5 || !slices.Equal(p.Oversized, []string{"conventions.md"}) {
		t.Errorf("previewDream = %+v, want Archivable %d, SessionRotate 5, Oversized [conventions.md]", p, len(overflow))
	}
	for path, n := range reads {
		if n != 1 {
			t.Errorf("previewDream read %s %d times, want once", path, n)
		}
	}
}

// The dry-run report pluralizes sessionHistory rotations the way the package's other counts do.
func TestDreamReportPluralizesRotatedEntries(t *testing.T) {
	got := dreamPreview{Dir: "d", SessionRotate: 3}.Report()
	if !strings.Contains(got, "would rotate 3 sessionHistory entr(ies) to archive/\n") {
		t.Fatalf("Report() = %q, want the entr(ies) rotation line", got)
	}
}

// TestPreviewDreamReportsUnreadableBankFile: a bank file that cannot be read is named, never "nothing to consolidate".
func TestPreviewDreamReportsUnreadableBankFile(t *testing.T) {
	tempConfig(t)
	locked := Path("decisionLog.md")
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == locked {
			return nil, fs.ErrPermission
		}
		return orig(p)
	}
	t.Cleanup(func() { readFile = orig })
	got := previewDream().Report()
	if !strings.Contains(got, "home decisionLog.md: permission denied — unreadable") || strings.Contains(got, "nothing to consolidate") {
		t.Errorf("dry run hid the unreadable bank file:\n%s", got)
	}
}

// Given a peer dreamed after this worker's due check, When it takes the lock, Then it skips and leaves the stamp.
func TestDreamIfDueSkipsUnderLockWhenPeerJustDreamed(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	peer := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := stampDream(peer, 0); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) {
		t.Error("a second dream ran right after a peer's")
		return "stub", nil
	}
	var out strings.Builder
	if err := dream(dreamOptions{CWD: root, Run: run, Out: &out, IfDue: true}); err != nil {
		t.Fatalf("dream = %v, want nil", err)
	}
	if got, _, _ := dreamStamp(); !got.Equal(peer) {
		t.Errorf("stamp = %v, want the peer's %v untouched", got, peer)
	}
	if !strings.Contains(out.String(), "not due") {
		t.Errorf("output %q lacks the not-due note", out.String())
	}
}

// Flat banks share one dir: every changed store file is listed once, none dropped, none repeated per bank.
func TestDreamChangedFlatListsEachFileOnce(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), "knowledge--topic.md"), "x\n")
	got := dreamChanged(time.Time{})
	for _, want := range []string{"progress.md", "knowledge--topic.md"} {
		if n := countBase(got, want); n != 1 {
			t.Errorf("dreamChanged lists %s %d times, want 1: %v", want, n, got)
		}
	}
}

func countBase(paths []string, base string) int {
	n := 0
	for _, p := range paths {
		if filepath.Base(p) == base {
			n++
		}
	}
	return n
}

func TestDreamRetryBackoff(t *testing.T) {
	for fails, want := range map[int]time.Duration{1: 6 * time.Hour, 2: 12 * time.Hour, 3: 24 * time.Hour, 9: 24 * time.Hour} {
		if got := retryAfter(fails); got != want {
			t.Errorf("retryAfter(%d) = %v, want %v", fails, got, want)
		}
	}
	isolatedConfig(t)
	for n := 1; n <= 3; n++ {
		if err := stampDream(time.Now(), n); err != nil {
			t.Fatal(err)
		}
		if got := dreamFailures(); got != n {
			t.Fatalf("dreamFailures = %d, want %d", got, n)
		}
	}
	if err := stampDream(time.Now(), 0); err != nil || dreamFailures() != 0 {
		t.Fatalf("success must reset the count: %v", err)
	}
}

func TestDreamRunLineIsTruthfulForAFailedRun(t *testing.T) {
	if got := dreamRunLine("claude", nil, false); got != "memory dream: ran on claude" {
		t.Errorf("ok line = %q", got)
	}
	got := dreamRunLine("claude", errors.New("Not logged in\nexit 3"), false)
	if strings.Contains(got, "ran on") || got != "memory dream: failed on claude: Not logged in" {
		t.Errorf("failed line = %q", got)
	}
}

// Every dream run leaves one record: a failed run, then a not-due run, read back newest first.
func TestDreamRecordsEveryRun(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string, string) (string, error) { return "stub", errors.New("boom\nsecond line") }
	idle := func(context.Context, string, string) (string, error) { return "stub", nil }
	runs := []dreamOptions{
		{CWD: root, Run: fail, Trigger: "manual", Forced: true},
		{CWD: root, Run: idle, IfDue: true, Trigger: "schedule"},
	}
	for _, o := range runs {
		_ = dream(o)
	}
	got, err := LastDreamResults(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("LastDreamResults = %v, %v; want 2 records", got, err)
	}
	brief := func(r DreamResult) string {
		return fmt.Sprintf("%s|%s|%s|forced=%t|log=%t", r.Trigger, r.Outcome, r.Reason, r.Forced, r.Log != "")
	}
	if g := brief(got[0]) + " / " + brief(got[1]); g != "schedule|noop|not due|forced=false|log=false / manual|failed|boom|forced=true|log=true" {
		t.Errorf("records = %s", g)
	}
}

func TestDreamLogsAreUniqueAndPruned(t *testing.T) {
	isolatedConfig(t)
	dir := paths.MemoryStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range dreamLogKeep + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("dream-20200101-000000.%03d-old.log", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := openDreamLog()
	if err != nil {
		t.Fatal(err)
	}
	b, err := openDreamLog()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if a.Name() == b.Name() {
		t.Errorf("same log name %s", a.Name())
	}
	if logs, _ := filepath.Glob(filepath.Join(dir, "dream-[0-9]*.log")); len(logs) != dreamLogKeep {
		t.Errorf("%d logs kept, want %d", len(logs), dreamLogKeep)
	}
}
