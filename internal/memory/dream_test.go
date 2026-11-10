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
)

// A failed run's retry deadline is independent of changes to either bank.
func TestPersistenceFailedDreamRetriesWithOldBanks(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("agent unavailable")
	run := func(context.Context, string, string) (string, error) { return "stub", failure }
	if err := dream(dreamOptions{CWD: root, Run: run}); !errors.Is(err, failure) {
		t.Fatalf("Dream = %v, want agent failure", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for _, dir := range []string{Dir(root), HomeBankDir()} {
		for name := range templates() {
			path := filepath.Join(dir, name)
			write(t, path, templates()[name])
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		if got := NewestMtime(dir); got.After(old) {
			t.Fatalf("bank fixture has recent mtime: %v", got)
		}
	}
	if DreamDue(root, now.Add(dreamRetry-time.Minute)) {
		t.Error("failed dream is due before six-hour backoff")
	}
	if !DreamDue(root, now.Add(dreamRetry+time.Minute)) {
		t.Error("failed dream with two-day-old banks is not due after six hours")
	}
}

// TestPreviewDreamReadsEachBankFileOnce: the cap, archive and rotation previews share one read per typed file.
func TestPreviewDreamReadsEachBankFileOnce(t *testing.T) {
	isolatedConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	dir := Dir(root)
	var conv, hist strings.Builder
	conv.WriteString("# Conventions\n")
	for i := range 90 {
		fmt.Fprintf(&conv, "\n- Feedback: rule %d", i)
	}
	hist.WriteString("# Session History\n")
	for i := range sessionHistoryMaxEntries + 5 {
		fmt.Fprintf(&hist, "\n## 2026-10-01 — session %d\n\nbody\n", i)
	}
	write(t, filepath.Join(dir, "conventions.md"), conv.String()+"\n")
	write(t, filepath.Join(dir, "sessionHistory.md"), hist.String())
	_, _, overflow, err := splitOverflow(filepath.Join(dir, "conventions.md"), archiveCaps["conventions.md"])
	if err != nil || len(overflow) == 0 {
		t.Fatalf("fixture has no conventions overflow: %v", err)
	}
	reads := countBankReads(t)
	p := previewDream(root)
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
	root := repoRoot(t, "proj")
	locked := filepath.Join(Dir(root), "decisionLog.md")
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == locked {
			return nil, fs.ErrPermission
		}
		return orig(p)
	}
	t.Cleanup(func() { readFile = orig })
	got := previewDream(root).Report()
	if !strings.Contains(got, "local decisionLog.md: permission denied — unreadable") || strings.Contains(got, "nothing to consolidate") {
		t.Errorf("dry run hid the unreadable bank file:\n%s", got)
	}
}
