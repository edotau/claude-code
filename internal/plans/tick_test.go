package plans

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
)

func userTickPlan(t *testing.T, body string) string {
	t.Helper()
	hermetic(t)
	p := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, p, body)
	return p
}

// TestTickRangeSkipsDraftMirror is the regression test: a range tick never lands on an unapproved draft's mirror.
func TestTickRangeSkipsDraftMirror(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	clockAt(t, "2026-01-03")
	repo := t.TempDir()
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base := strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "add", "a.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "a")

	home := Home(repo)
	approved, draft := filepath.Join(home, "2026-01-03-approved.md"), filepath.Join(home, "2026-01-02-draft.md")
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

// landRepo commits each file on its own and returns the repo with one Commit (7-char sha, files) per file.
func landRepo(t *testing.T, files ...string) (string, []Commit) {
	t.Helper()
	repo := t.TempDir()
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	var out []Commit
	for _, f := range files {
		writeFile(t, filepath.Join(repo, f), "x")
		hosttest.In(t, repo, "add", f)
		hosttest.In(t, repo, "commit", "-q", "-m", f)
		out = append(out, Commit{Sha: strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "--short=7", "HEAD")), Files: []string{f}})
	}
	return repo, out
}

// A task owning three files landed in three commits ticks on the third, with the last sha and no partial note.
func TestTickAccumulatesAcrossCommits(t *testing.T) {
	hermetic(t)
	repo, cs := landRepo(t, "a.go", "b.go", "c.go", "d.go")
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Split owns: a.go, b.go, c.go\n- [ ] 1.2 Pair owns: c.go, d.go\n")
	for i, c := range cs[:2] {
		if _, _, err := Tick(repo, plan, c.Sha, c.Files); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, plan); strings.Contains(got, "[x]") || strings.Count(got, "partly landed") != 1 {
			t.Fatalf("after commit %d want one partial note, no tick:\n%s", i+1, got)
		}
	}
	ticked, _, err := Tick(repo, plan, cs[2].Sha, cs[2].Files)
	if err != nil || len(ticked) != 1 {
		t.Fatalf("third commit: ticked=%v err=%v", ticked, err)
	}
	got := readFile(t, plan)
	if !strings.Contains(got, "- [x] 1.1 Split owns: a.go, b.go, c.go ("+cs[2].Sha+")\n") {
		t.Fatalf("task 1.1 not ticked with the last sha and no note:\n%s", got)
	}
	if !strings.Contains(got, "- [ ] 1.2 Pair owns: c.go, d.go <!-- partly landed: "+cs[2].Sha+" -->") {
		t.Fatalf("the commit covering two tasks must leave a note on the one it did not complete:\n%s", got)
	}
	if _, _, err := Tick(repo, plan, cs[3].Sha, cs[3].Files); err != nil || !strings.Contains(readFile(t, plan), "- [x] 1.2 Pair owns: c.go, d.go ("+cs[3].Sha+")\n") {
		t.Fatalf("1.2 must tick on its second commit:\n%s", readFile(t, plan))
	}
}

// TickCommit run twice for one commit (the verb, then the hook) changes the plan once; an older plan ticks too.
func TestTickCommitIsIdempotentAcrossPlans(t *testing.T) {
	cfg := hermetic(t)
	repo, cs := landRepo(t, "a.go", "b.go")
	home := Home(repo)
	oldP, newP := filepath.Join(home, "2026-10-01-old.md"), filepath.Join(home, "2026-10-09-new.md")
	writeFile(t, oldP, "- [ ] 1.1 Old owns: a.go\n")
	writeFile(t, newP, "- [ ] 2.1 New owns: b.go, zzz.go\n")
	_ = cfg
	files := []string{"a.go", "b.go"}
	res, _, err := TickCommit(repo, cs[1].Sha, files)
	if err != nil || len(res) != 2 {
		t.Fatalf("first call: %+v err=%v", res, err)
	}
	snap := readFile(t, oldP) + readFile(t, newP)
	if !strings.Contains(snap, "[x] 1.1") || !strings.Contains(snap, "partly landed: "+cs[1].Sha) {
		t.Fatalf("older plan must tick and newer must note:\n%s", snap)
	}
	if res, _, err = TickCommit(repo, cs[1].Sha, files); err != nil || len(res) != 0 || readFile(t, oldP)+readFile(t, newP) != snap {
		t.Fatalf("second call must change nothing: %+v err=%v", res, err)
	}
}

// A task with no owns: ticks, reopens and strikes by hand; a two-line task keeps every line within 120 columns.
func TestPlanTaskOps(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	long := strings.Repeat("word ", 20) + "tail"
	writeFile(t, plan, "- [ ] 1.1 Manual step <!-- working: x -->\n- [ ] 1.2 Two lines "+long+"\n  second line "+long+"\n- [ ] 1.3 Next\n")
	if err := MarkDone(plan, "1.1", "abc1234"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, plan); !strings.HasPrefix(got, "- [x] 1.1 Manual step (abc1234)\n") {
		t.Fatalf("done:\n%s", got)
	}
	if err := Reopen(plan, "1.1"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, plan); !strings.HasPrefix(got, "- [ ] 1.1 Manual step\n") {
		t.Fatalf("reopen must drop the tick and the sha:\n%s", got)
	}
	if err := Strike(plan, "1.2", "folded into 1.1"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, plan)
	for l := range strings.SplitSeq(got, "\n") {
		if utf8.RuneCountInString(l) > 120 {
			t.Fatalf("line over 120 columns: %q", l)
		}
	}
	p := parseBytes(plan, []byte(got))
	if len(p.Tasks) != 3 || !p.Tasks[1].Dropped || p.Tasks[1].ID != "1.2" || p.Tasks[2].Dropped || !strings.HasSuffix(strings.TrimSpace(got[:strings.Index(got, "- [ ] 1.3")]), "superseded: folded into 1.1") {
		t.Fatalf("strike shape:\n%s", got)
	}
	if s := summarize(p); s.Total != 2 || s.Dropped != 1 {
		t.Fatalf("summary %+v", s)
	}
	if Strike(plan, "1.1", "") == nil {
		t.Fatal("a strike needs a reason")
	}
	if err := MarkDone(plan, "1.3", ""); err != nil || Strike(plan, "1.3", "x") == nil {
		t.Fatalf("a ticked task must refuse a strike (done err=%v)", err)
	}
}

// TaskPlans lists every candidate so the CLI can refuse a non-unique id.
func TestTaskPlansListsEveryHolder(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-a.md"), "- [ ] 1.1 One\n")
	writeFile(t, filepath.Join(home, "2026-10-02-b.md"), "- [ ] 1.1 Other\n- [x] 1.2 Done\n")
	open := func(t Task) bool { return !t.Done && !t.Dropped }
	if got := TaskPlans(home, nil, "1.1", open); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got := TaskPlans(home, nil, "1.2", open); len(got) != 0 {
		t.Fatalf("a ticked task is not open: %v", got)
	}
}

// Only a plan dated within the window (boundary day included) or a task claimed here is ticked by a commit.
func TestEligibilityWindowAndClaim(t *testing.T) {
	hermetic(t)
	clockAt(t, "2026-11-10")
	repo, cs := landRepo(t, "a.go")
	home := Home(repo)
	names := map[string]string{"old": "2026-11-02-old.md", "edge": "2026-11-03-edge.md", "new": "2026-11-08-new.md"}
	for _, n := range names {
		writeFile(t, filepath.Join(home, n), "- [ ] 1.1 Task owns: a.go, zzz.go\n")
	}
	if _, _, err := TickCommit(repo, cs[0].Sha, cs[0].Files); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{"old": false, "edge": true, "new": true} {
		if got := strings.Contains(readFile(t, filepath.Join(home, names[k])), "partly landed"); got != want {
			t.Errorf("%s plan annotated=%v, want %v", k, got, want)
		}
	}
	claimed := filepath.Join(home, names["old"])
	writeFile(t, claimed, "- [ ] 1.1 Task owns: a.go, zzz.go <!-- working: x · worktree main -->\n")
	if _, _, err := TickCommit(repo, cs[0].Sha, cs[0].Files); err != nil || !strings.Contains(readFile(t, claimed), "partly landed") {
		t.Fatalf("a claimed task in an old plan must be annotated (err=%v):\n%s", err, readFile(t, claimed))
	}
}

// An explicit plan is always eligible, whatever its age; a range without one skips the old plan.
func TestTickRangeExplicitPlanIgnoresAge(t *testing.T) {
	hermetic(t)
	clockAt(t, "2026-11-10")
	repo, base := tickRangeRepo(t)
	old := filepath.Join(Home(repo), "2026-01-01-old.md")
	writeFile(t, old, "- [ ] 1.1 Task owns: internal/a/a.go\n")
	rng := base + "..HEAD"
	var out strings.Builder
	if err := TickRange(context.Background(), repo, "", rng, "", nil, false, &out); err != nil || strings.Contains(readFile(t, old), "[x]") {
		t.Fatalf("old plan ticked without being named (err=%v): %s", err, out.String())
	}
	if err := TickRange(context.Background(), repo, "", rng, old, nil, false, io.Discard); err != nil || !strings.Contains(readFile(t, old), "[x] 1.1") {
		t.Fatalf("explicit old plan not ticked (err=%v):\n%s", err, readFile(t, old))
	}
}
