package plans

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	tickAt1 = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	tickAt2 = tickAt1.Add(time.Hour)
)

func userTickPlan(t *testing.T, body string) string {
	t.Helper()
	hermetic(t)
	p := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, p, body)
	return p
}

func readPlan(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Spanning: tick then untick leaves exactly one marker, keeps sha/claim/partial, and Title is unchanged.
func TestUserTickMarkerReplacedAndMetadataKept(t *testing.T) {
	p := userTickPlan(t, "# P\n\n- [ ] 1.1 Alpha owns: a.go (abc1234) <!-- working: bob --> <!-- partly landed: a.go -->\n- [ ] 1.2 Beta\n")
	before, _ := Parse(p)
	if err := UserTick(p, 3, before.Tasks[0].Title, true, tickAt1); err != nil {
		t.Fatal(err)
	}
	got := readPlan(t, p)
	if !strings.Contains(got, "- [x] 1.1 Alpha") || !strings.Contains(got, "<!-- user: ticked 2026-10-31T09:00:00Z -->") {
		t.Fatalf("tick not written:\n%s", got)
	}
	if err := UserTick(p, 3, before.Tasks[0].Title, false, tickAt2); err != nil {
		t.Fatal(err)
	}
	got = readPlan(t, p)
	if strings.Count(got, "<!-- user:") != 1 || !strings.Contains(got, "<!-- user: unticked 2026-10-31T10:00:00Z -->") ||
		!strings.Contains(got, "- [ ] 1.1 Alpha") {
		t.Fatalf("untick did not replace the marker:\n%s", got)
	}
	after, _ := Parse(p)
	a, b := after.Tasks[0], before.Tasks[0]
	if a.Title != b.Title || a.Sha != "abc1234" || !strings.Contains(a.Claim, "working: bob") || len(a.Partial) != 1 ||
		a.UserMark != "unticked 2026-10-31T10:00:00Z" {
		t.Fatalf("metadata lost or title changed: before=%+v after=%+v", b, a)
	}
	if strings.Contains(strings.Split(got, "\n")[3], "user:") {
		t.Fatalf("neighbouring task touched:\n%s", got)
	}
}

// Corner cases: stale title, out-of-range lines and a fenced checkbox all report ErrTaskMoved and leave the file alone.
func TestUserTickRejectsMovedTasks(t *testing.T) {
	body := "- [ ] 2.1 Real\n\n```\n- [ ] 9.9 Fenced\n```\n"
	p := userTickPlan(t, body)
	cases := []struct {
		name  string
		line  int
		title string
	}{
		{"wrong title", 1, "2.1 Other"},
		{"line zero", 0, "2.1 Real"},
		{"past end", 99, "2.1 Real"},
		{"negative", -3, "2.1 Real"},
		{"inside fence", 4, "9.9 Fenced"},
	}
	for _, c := range cases {
		if err := UserTick(p, c.line, c.title, true, tickAt1); !errors.Is(err, ErrTaskMoved) {
			t.Errorf("%s: err = %v, want ErrTaskMoved", c.name, err)
		}
	}
	if got := readPlan(t, p); got != body {
		t.Fatalf("file changed on rejected ticks:\n%s", got)
	}
}

// An ID-less task ticks by line+title, and a multi-line item gets one marker on its last line.
func TestUserTickEmptyIDAndContinuation(t *testing.T) {
	p := userTickPlan(t, "- [ ] Plain task\n  continued here\n- [ ] Next\n")
	pl, _ := Parse(p)
	if pl.Tasks[0].ID != "" {
		t.Fatalf("fixture has an ID: %+v", pl.Tasks[0])
	}
	if err := UserTick(p, 1, pl.Tasks[0].Title, true, tickAt1); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(readPlan(t, p), "\n")
	if !strings.HasPrefix(lines[0], "- [x] Plain task") || strings.Contains(lines[0], "user:") ||
		!strings.Contains(lines[1], "<!-- user: ticked") || strings.Contains(lines[2], "user:") {
		t.Fatalf("marker misplaced: %q", lines)
	}
}

// TestTickRangeSkipsDraftMirror is the regression test: a range tick never lands on an unapproved draft's mirror.
func TestTickRangeSkipsDraftMirror(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.go")
	git("commit", "-q", "-m", "a")

	home := Home(repo)
	approved, draft := filepath.Join(home, "2026-01-01-approved.md"), filepath.Join(home, "2026-01-02-draft.md")
	for _, p := range []string{approved, draft} {
		writeFile(t, p, "- [ ] 1.1 Task owns: a.go\n")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(approved, old, old); err != nil { // the draft is newer, so only the skip keeps it out
		t.Fatal(err)
	}
	drafts := []string{draft}

	if err := TickRange(context.Background(), repo, "", base+"..HEAD", "", drafts, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(approved); !strings.Contains(string(b), "- [x] 1.1") {
		t.Errorf("approved plan not ticked:\n%s", b)
	}
	if b, _ := os.ReadFile(draft); strings.Contains(string(b), "[x]") {
		t.Errorf("draft mirror ticked:\n%s", b)
	}
	if err := TickRange(context.Background(), repo, "", base+"..HEAD", draft, drafts, true, io.Discard); err == nil {
		t.Error("--plan naming a draft mirror must be refused")
	}
}
