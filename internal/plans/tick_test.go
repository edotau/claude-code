package plans

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	hosttest.In(t, repo, "commit", "-q", "-m", "a\n\nTask: 2026-01-03-approved 1.1")

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

// A named commit that touches only some owned files still ticks, and its note names the untouched ones.
func TestNamedSubsetCommitTicksAndNotes(t *testing.T) {
	hermetic(t)
	repo, cs := landRepo(t, "a.go")
	plan := filepath.Join(t.TempDir(), "2026-10-05-p.md")
	writeFile(t, plan, "- [ ] 1.1 Pair owns: a.go, b.go\n- [ ] 1.2 Bare\n")
	c := Commit{Sha: cs[0].Sha, Files: cs[0].Files, Message: "feat: a\r\n\r\nTask: 2026-10-05-p 1.1\r\nTask: 2026-10-05-p 1.2\r\n"}
	res, err := tickCommits(repo, plan, []Commit{c}, false, nil)
	if err != nil || len(res) != 1 || len(res[0].Ticked) != 2 || len(res[0].Notes) != 1 || !strings.Contains(res[0].Notes[0], "owns: b.go") {
		t.Fatalf("results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); !strings.Contains(got, "[x] 1.1") || !strings.Contains(got, "[x] 1.2 Bare ("+cs[0].Sha+")") {
		t.Fatalf("a named task with missing or no owns must tick:\n%s", got)
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
	if err := Strike(plan, "1.3", "why"); err != nil {
		t.Fatal(err)
	}
	if err := Reopen(plan, "1.3"); err != nil || readFile(t, plan) != got {
		t.Fatalf("reopen must undo a one-line strike byte for byte (err=%v):\n%s", err, readFile(t, plan))
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
	res, _, err := TickCommit(repo, cs[0].Sha, cs[0].Files)
	if err != nil || len(res) != 2 {
		t.Fatalf("eligible candidates: %+v, %v", res, err)
	}
	for _, n := range names {
		if strings.Contains(readFile(t, filepath.Join(home, n)), "partly landed") {
			t.Fatal("unnamed task annotated")
		}
	}
	claimed := filepath.Join(home, names["old"])
	writeFile(t, claimed, "- [ ] 1.1 Task owns: a.go, zzz.go <!-- working: x · worktree main -->\n")
	res, _, err = TickCommit(repo, cs[0].Sha, cs[0].Files)
	if err != nil || len(res) != 1 || res[0].Plan != claimed {
		t.Fatalf("only the newly claimed plan is a new candidate: %+v, %v", res, err)
	}
}

// A trailer names its plan, so a task in an old plan ticks without --plan; unnamed coverage there stays silent.
func TestTickRangeNamedTaskIgnoresPlanAge(t *testing.T) {
	hermetic(t)
	clockAt(t, "2026-11-10")
	repo, base := tickRangeRepo(t)
	old := filepath.Join(Home(repo), "2026-01-01-old.md")
	unnamed := filepath.Join(Home(repo), "2026-01-02-unnamed.md")
	writeFile(t, old, "- [ ] 1.1 Task owns: internal/a/a.go\n")
	writeFile(t, unnamed, "- [ ] 1.1 Task owns: internal/a/a.go\n")
	var out strings.Builder
	if err := TickRange(context.Background(), repo, "", base+"..HEAD", "", nil, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, old), "[x] 1.1") || strings.Contains(readFile(t, unnamed), "[x]") || strings.Contains(out.String(), "unnamed") {
		t.Fatalf("named old plan must tick and the unnamed old plan stay silent: %s", out.String())
	}
}

// gitRepo is a repo on branch br with one seed commit.
func gitRepo(t *testing.T, br string) string {
	t.Helper()
	repo := t.TempDir()
	hosttest.In(t, repo, "init", "-q", "-b", br)
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "seed")
	return repo
}

// Trunk is origin/HEAD, else init.defaultBranch, main or master, whichever exists; with none, nothing is trunk.
func TestTrunkTipFailsClosedWithoutTrunk(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	repo := gitRepo(t, "trunk")
	if _, on, err := trunkTip(ctx, repo); !errors.Is(err, ErrNoTrunk) || on {
		t.Fatalf("a repo with no main, master or default must not guess its checked-out branch: on=%v err=%v", on, err)
	}
	hosttest.In(t, repo, "branch", "main")
	if _, on, err := trunkTip(ctx, repo); err != nil || on {
		t.Fatalf("trunk beside an existing main is not the landing branch: on=%v err=%v", on, err)
	}
	hosttest.In(t, repo, "update-ref", "refs/remotes/origin/old", "HEAD")
	hosttest.In(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/old")
	hosttest.In(t, repo, "checkout", "-q", "main")
	if _, on, err := trunkTip(ctx, repo); err != nil || !on {
		t.Fatalf("origin/HEAD naming a branch with no local copy must fall through to main: on=%v err=%v", on, err)
	}
	hosttest.In(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/gone")
	hosttest.In(t, repo, "checkout", "-q", "main")
	if tip, on, err := trunkTip(ctx, repo); err != nil || !on || tip == "" {
		t.Fatalf("a stale origin/HEAD must fall through to main: tip=%q on=%v err=%v", tip, on, err)
	}
	lane := filepath.Join(t.TempDir(), "lane")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "lane", lane)
	if _, on, _ := trunkTip(ctx, lane); on {
		t.Fatal("a linked worktree never lands")
	}
	hosttest.In(t, repo, "checkout", "-q", "--detach")
	if _, on, _ := trunkTip(ctx, repo); on {
		t.Fatal("a detached HEAD lands nothing")
	}
}

// A commit made in a lane ticks nothing; cherry-picked, rebased or squashed onto trunk, its trailer ticks it once.
func TestNamedTaskTicksWhenItsCommitLands(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	plan := filepath.Join(Home(repo), "2026-10-05-p.md")
	writeFile(t, plan, "- [ ] 1.1 One owns: a.go\n- [ ] 1.2 Two owns: b.go\n- [ ] 1.3 Three owns: c.go\n")
	lane := filepath.Join(t.TempDir(), "lane")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "lane", lane)
	for i, f := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, filepath.Join(lane, f), "x")
		hosttest.In(t, lane, "add", f)
		hosttest.In(t, lane, "commit", "-q", "-m", "feat: "+f+"\n\nTask: 2026-10-05-p 1."+string(rune('1'+i)))
	}
	laneHead := strings.TrimSpace(hosttest.In(t, lane, "rev-parse", "HEAD"))
	if res, _, err := TickCommit(lane, laneHead, nil); err != nil || len(res) != 0 || strings.Contains(readFile(t, plan), "[x]") {
		t.Fatalf("a lane commit must tick nothing: %+v %v", res, err)
	}
	hosttest.In(t, repo, "cherry-pick", "lane~2")
	hosttest.In(t, repo, "merge", "--squash", "lane~1")
	hosttest.In(t, repo, "commit", "-q", "-m", "feat: squash\n\nTask: 2026-10-05-p 1.2")
	got := ""
	for sha := range strings.FieldsSeq(hosttest.In(t, repo, "rev-list", "--reverse", "-2", "HEAD")) {
		if _, _, err := TickCommit(repo, sha, nil); err != nil {
			t.Fatal(err)
		}
		got = readFile(t, plan)
	}
	if !strings.Contains(got, "[x] 1.1") || !strings.Contains(got, "[x] 1.2") || strings.Contains(got, "[x] 1.3") {
		t.Fatalf("cherry-pick and squash keep their trailers, the unlanded task stays open:\n%s", got)
	}
	var out strings.Builder
	if err := TickRange(context.Background(), lane, "lane", "main~1..HEAD", "", nil, false, &out); !errors.Is(err, ErrNotOnTrunk) {
		t.Fatalf("a range tick from a lane must say why nothing ticked: %v", err)
	}
}

// A requested task id must resolve to exactly one open task, or the refusal says why and what is near.
func TestTaskTrailersRefusals(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	home := Home(repo)
	writeFile(t, filepath.Join(home, "2026-10-05-a.md"), "- [ ] 1.1 One\n- [x] 1.2 Done\n- [ ] ~~1.3 Struck~~ — superseded: no\n- [ ] Phase 2.1 Spaced\n")
	writeFile(t, filepath.Join(home, "2026-10-04-b.md"), "- [ ] 1.1 Twin\n")
	writeFile(t, filepath.Join(home, "2026-01-01-old.md"), "- [ ] 9.1 Old\n")
	cases := []struct{ id, plan, want string }{
		{"1.1", "", "2026-10-04-b and 2026-10-05-a"},
		{"1.1", filepath.Join(home, "2026-10-04-b.md"), "\n\nTask: 2026-10-04-b 1.1"},
		{"1.2", "", "already ticked or struck in 2026-10-05-a"},
		{"1.2", "", "plan task '1.2' --reopen --plan '"},
		{"1.3", "", "already ticked or struck in 2026-10-05-a"},
		{"1.4", "", "nearest ids: 1.1"},
		{"Phase 2.1", "", "\n\nTask: 2026-10-05-a Phase 2.1"},
		{"9.1", "", "--plan <file> reaches any"},
		{"9.1", filepath.Join(home, "2026-01-01-old.md"), "\n\nTask: 2026-01-01-old 9.1"},
	}
	for _, c := range cases {
		got, err := TaskTrailers(repo, c.plan, []string{c.id})
		if msg := got + fmt.Sprint(err); !strings.Contains(msg, c.want) {
			t.Errorf("%q --plan %q: got %q, %v; want %q", c.id, c.plan, got, err, c.want)
		}
	}
}

func TestTaskTrailerGitDifferential(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		body   string
		strict bool
	}{
		{"Task: p 1\n continuation prose\n", false},
		{"Task:  p 1\n", false}, {"Task \t: p   1\n", false},
		{"Task: p\n 1\n", false}, {"Task: p 1\r\nSigned-off-by: A <a@b>\r\n", false},
		{"Signed-off-by: A <a@b>\nTask: p 1", false},
		{"Task: p 1\nTask: p 2\n", false},
		{"Task:\n p 1\n", false}, {"Foo:\nTask: p 1\n", false}, {"123: x\nTask: p 1\n", false},
		{"prose\nSigned-off-by: A <a@b>\nTask: p 1\n", true},
		{"> Task: p 1\nSigned-off-by: A <a@b>\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			message := "subject\n\n" + tc.body
			values := gitTaskValues(t, git, message)
			if got := taskTrailerValues(message); !tc.strict && !slices.Equal(got, values) {
				t.Errorf("Go values=%q git=%q", got, values)
			}
			for i, value := range values {
				values[i] = strings.Join(strings.Fields(value), " ")
			}
			for _, id := range []string{"1", "2", "1 continuation prose"} {
				want := !tc.strict && slices.Contains(values, "p "+id)
				if got := namesTask(message, "p.md", id); got != want {
					t.Errorf("id=%q Go=%v git=%q strict=%v", id, got, values, tc.strict)
				}
			}
		})
	}
}

func gitTaskValues(t *testing.T, git, message string) []string {
	t.Helper()
	cmd := exec.Command(git, "-c", "trailer.separators=:", "interpret-trailers", "--parse")
	cmd.Stdin = strings.NewReader(message)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Task") {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}
