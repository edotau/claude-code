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
		if got := newestMtime(dir); got.After(old) {
			t.Fatalf("bank fixture has recent mtime: %v", got)
		}
	}
	if dreamDue(root, now.Add(dreamRetry-time.Minute)) {
		t.Error("failed dream is due before six-hour backoff")
	}
	if !dreamDue(root, now.Add(dreamRetry+time.Minute)) {
		t.Error("failed dream with two-day-old banks is not due after six hours")
	}
}

// A dream whose every tool call the gate refused, with changed files left unedited, is a failed run that retries.
func TestDreamGateRefusalsWithoutEditsStampForRetry(t *testing.T) {
	for _, tc := range []struct {
		name         string
		blocks       int
		edit, failed bool
	}{
		{"blocked and nothing edited", 2, false, true},
		{"blocked then edited", 2, true, false},
		{"nothing to merge", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedConfig(t)
			t.Setenv(UpdateChildEnv, "")
			t.Setenv(dreamChildEnv, "")
			root := repoRoot(t, "proj")
			if _, err := Init(root); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(filepath.Join(Dir(root), "conventions.md"), old, old); err != nil {
				t.Fatal(err)
			}
			run := func(ctx context.Context, _, _ string) (string, error) {
				NoteGateBlocks(ctx, tc.blocks)
				if tc.edit {
					write(t, filepath.Join(Dir(root), "conventions.md"), templates()["conventions.md"]+"\n- merged\n")
				}
				return "stub", nil
			}
			err := dream(dreamOptions{CWD: root, Run: run})
			if (err != nil) != tc.failed {
				t.Fatalf("dream error = %v, want failure %v", err, tc.failed)
			}
			if _, failed, ok := dreamStamp(root); !ok || failed != tc.failed {
				t.Errorf("stamp failed = %v (present %v), want %v", failed, ok, tc.failed)
			}
			if due := dreamDue(root, time.Now().Add(dreamRetry+time.Minute)); due != tc.failed {
				t.Errorf("dreamDue after the retry wait = %v, want %v", due, tc.failed)
			}
		})
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

// Given a peer dreamed after this worker's due check, When the worker takes the lock, Then it skips and leaves the stamp.
func TestDreamIfDueSkipsUnderLockWhenPeerJustDreamed(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	peer := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := stampDream(root, peer, false); err != nil {
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
	if got, _, _ := dreamStamp(root); !got.Equal(peer) {
		t.Errorf("stamp = %v, want the peer's %v untouched", got, peer)
	}
	if !strings.Contains(out.String(), "not due") {
		t.Errorf("output %q lacks the not-due note", out.String())
	}
}
