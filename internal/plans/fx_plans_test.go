package plans

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const openTask = "# Plan: x\n\n- [ ] 1.1 Task owns: a/b.go%s\n"

// A symlinked .md (pointing outside plans/) is never summarized.
func TestStatusSkipsSymlink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "real.md"), "- [ ] 1.1 T owns: a\n")
	writeFile(t, filepath.Join(dir, "own.md"), "- [ ] 1.1 T owns: a\n")
	if err := os.Symlink(filepath.Join(outside, "real.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	sums, err := Status(dir)
	if err != nil || len(sums) != 1 || filepath.Base(sums[0].Path) != "own.md" {
		t.Fatalf("Status = %+v, %v; want only own.md", sums, err)
	}
}

// An unapproved mirrored draft named in skip is never the tick target.
func TestActiveSkipsDraft(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "2026-10-29-draft.md")
	writeFile(t, draft, "# Plan: d\n\n- [ ] 1.1 T owns: a\n")
	if got, ok := Active(dir, "", []string{draft}); ok {
		t.Fatalf("Active picked skipped draft %s", got)
	}
	if got, ok := Active(dir, "", nil); !ok || got != draft {
		t.Fatalf("Active without skip = %q, %v", got, ok)
	}
}

// The plan whose open-task claim names the worktree beats a newer unclaimed plan.
func TestActivePrefersWorktreeClaim(t *testing.T) {
	dir := t.TempDir()
	claimed := filepath.Join(dir, "2026-10-01-claimed.md")
	newer := filepath.Join(dir, "2026-10-02-newer.md")
	writeFile(t, claimed, "- [ ] 1.1 T owns: a <!-- working: bob · worktree fx-1 -->\n")
	writeFile(t, newer, "- [ ] 1.1 T owns: a\n")
	now := time.Now()
	_ = os.Chtimes(claimed, now.Add(-time.Hour), now.Add(-time.Hour))
	_ = os.Chtimes(newer, now, now)
	if got, _ := Active(dir, "fx-1", nil); got != claimed {
		t.Fatalf("Active(fx-1) = %q, want the claimed plan", got)
	}
	if got, _ := Active(dir, "other", nil); got != newer {
		t.Fatalf("Active(other) = %q, want the newest plan", got)
	}
}

// Checkbox lines inside fenced code are not tasks.
func TestParseSkipsFencedCheckboxes(t *testing.T) {
	p := ParseBytes("p.md", []byte("- [ ] 1.1 Real owns: a\n\n```md\n- [ ] 1.2 Fake owns: b\n```\n\n~~~\n- [ ] 1.3 Fake\n~~~\n- [ ] 1.4 Real2 owns: c\n"))
	if len(p.Tasks) != 2 || p.Tasks[0].ID != "1.1" || p.Tasks[1].ID != "1.4" {
		t.Fatalf("tasks = %+v, want 1.1 and 1.4 only", p.Tasks)
	}
}
