package plans

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// hermetic points the harness config dir (plans.lock lives under StateDir) at a temp dir.
func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	clockAt(t, "2026-10-05")
	return dir
}

// clockAt fixes the recency clock at midday of day (YYYY-MM-DD) for the test.
func clockAt(t *testing.T, day string) {
	t.Helper()
	at, err := time.ParseInLocation(time.DateOnly, day, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	old := now
	now = func() time.Time { return at.Add(12 * time.Hour) }
	t.Cleanup(func() { now = old })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const tickPlan = `# Plan: tick fixture

Intro prose line.

- [ ] 1.1 Build the parser and writer
  spanning two lines owns: internal/plans/, internal/hooks/safety_*_test.go, internal/cli/cli.go (verb row only)
- [ ] 1.2 Claimed task owns: internal/claimed/y.go <!-- working: alice on it -->
- [x] 1.3 Done task owns: internal/done/z.go (deadbee)
`

// tickOK ticks path for sha and fails the test on error.
func tickOK(t *testing.T, path, sha string, files ...string) (ticked, notes []string) {
	t.Helper()
	ticked, notes, err := namedTestTick(t, "", path, sha, files)
	if err != nil {
		t.Fatal(err)
	}
	return ticked, notes
}

// wantBytes fails unless path holds exactly want.
func wantBytes(t *testing.T, path, label, want string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Fatalf("%s bytes:\ngot:\n%s\nwant:\n%s", label, got, want)
	}
}

// wantTicks fails unless exactly nTicked tasks ticked (the first containing tickedSub) with nNotes notes.
func wantTicks(t *testing.T, label string, ticked, notes []string, nTicked int, tickedSub string, nNotes int) {
	t.Helper()
	if len(ticked) != nTicked || len(notes) != nNotes || (nTicked > 0 && !strings.Contains(ticked[0], tickedSub)) {
		t.Fatalf("%s: ticked=%v notes=%v", label, ticked, notes)
	}
}

// TestTickSpanning ticks named tasks from a subset commit, then a claimed one, asserting exact bytes.
func TestTickSpanning(t *testing.T) {
	hermetic(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, path, tickPlan)

	// A named commit touching one of three owns ticks the task and notes the rest; the done 1.3 is skipped.
	ticked, notes := tickOK(t, path, "aaaaaaa", "internal/plans/plans.go")
	wantTicks(t, "subset", ticked, notes, 2, "1.1 Build", 2)
	if !strings.Contains(notes[0], "plans: 1.1 ticked without owns: internal/hooks/safety_*_test.go, internal/cli/cli.go") || !strings.Contains(notes[0], "plan task '1.1' --reopen --plan '") {
		t.Fatalf("note must list the untouched owned files and the reopen route: %v", notes)
	}
	want := strings.Replace(tickPlan, "- [ ] 1.1 Build", "- [x] 1.1 Build", 1)
	want = strings.Replace(want, "(verb row only)\n", "(verb row only) (aaaaaaa)\n", 1)
	want = strings.Replace(want, "- [ ] 1.2 Claimed task owns: internal/claimed/y.go <!-- working: alice on it -->\n",
		"- [x] 1.2 Claimed task owns: internal/claimed/y.go (aaaaaaa)\n", 1)
	wantBytes(t, path, "subset", want)
	assertRepeatShaIsNoop(t, path, want)
	if !strings.HasSuffix(want, "\n") {
		t.Fatal("fixture lost its trailing newline")
	}
}

// assertRepeatShaIsNoop re-ticks sha aaaaaaa: neither duplicates nor rewrites, so bytes and mtime stay put.
func assertRepeatShaIsNoop(t *testing.T, path, want string) {
	t.Helper()
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	ticked, notes := tickOK(t, path, "aaaaaaa", "internal/plans/tick.go")
	wantTicks(t, "repeat sha", ticked, notes, 0, "", 0)
	wantBytes(t, path, "repeat sha", want)
	if info, err := os.Stat(path); err != nil || info.ModTime().Unix() != stamp.Unix() {
		t.Fatalf("repeat sha rewrote file: mtime=%v err=%v", info.ModTime(), err)
	}
}

const claimsPlan = `# Plan: claims

- [ ] 2.1 First task owns: internal/a/x.go
- [ ] 2.2 Second task owns: internal/b/y.go <!-- working: carol -->
`

// claimsFixture writes claimsPlan to a temp plan file.
func claimsFixture(t *testing.T) string {
	t.Helper()
	hermetic(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, path, claimsPlan)
	return path
}

// TestClaimByIDAndTitle covers claim lookup by ID and by title prefix, appended at the task's last line.
func TestClaimByIDAndTitle(t *testing.T) {
	path := claimsFixture(t)
	if err := ClaimFree(path, "2.1", "working: dave"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, "owns: internal/a/x.go <!-- working: dave -->") {
		t.Fatalf("claim by id:\n%s", got)
	}
	if err := ClearClaimOwned(path, "2.1 First", "dave"); err != nil {
		t.Fatal(err)
	}
}

// TestClearClaimOwnedDropsCommentOnce: clearing drops the comment; clearing again leaves bytes untouched.
func TestClearClaimOwnedDropsCommentOnce(t *testing.T) {
	path := claimsFixture(t)
	if err := ClearClaimOwned(path, "2.2", "carol"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "- [ ] 2.2 Second task owns: internal/b/y.go\n") {
		t.Fatalf("clear claim:\n%s", got)
	}
	if err := ClearClaimOwned(path, "2.2", "carol"); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, path, "clear without claim", got)
}

// TestClaimRejections covers the rejected claim and clear requests.
func TestClaimRejections(t *testing.T) {
	path := claimsFixture(t)
	for _, tc := range []struct {
		name, taskID, text, wantErr string
		clear                       bool
	}{
		{"comment terminator", "2.1", "working: x --> y", "-->", false},
		{"bad verb", "2.1", "thinking: x", "must start with \"working:\"", false},
		{"unknown id", "9.9", "working: x", `no task "9.9"`, false},
		{"unknown clear", "Nope", "", `no task "Nope"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.clear {
				err = ClearClaimOwned(path, tc.taskID, "x")
			} else {
				err = ClaimFree(path, tc.taskID, tc.text)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestStatus counts ticks, partials and claims across the *.md files of a dir.
func TestStatus(t *testing.T) {
	hermetic(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "2026-10-29-main.md"), `# Plan: status

- [x] 1.1 Done owns: a/x.go (deadbee)
- [ ] 1.2 Partial one owns: b/y.go <!-- partly landed: aaaaaaa -->
- [ ] 1.3 Claimed owns: c/z.go <!-- working: alice on it -->
- [ ] 1.4 Plain owns: d/w.go
`)
	writeFile(t, filepath.Join(dir, "2026-10-28-done.md"), `# Plan: done

- [x] 2.1 One owns: e/f.go (aaaaaaa)
- [x] 2.2 Two owns: e/g.go (bbbbbbb)
`)
	writeFile(t, filepath.Join(dir, "notes.md"), "# Notes\n\nNo checklist here.\n")
	writeFile(t, filepath.Join(dir, "subdir", "nested.md"), "- [ ] 3.1 Nested owns: f/g.go\n")

	sums, err := Status(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("summaries=%v", sums)
	}
	if !strings.HasSuffix(sums[0].Path, "2026-10-28-done.md") || !strings.HasSuffix(sums[1].Path, "2026-10-29-main.md") {
		t.Fatalf("not sorted by path: %v", sums)
	}
	assertStatusDone(t, sums[0])
	assertStatusMain(t, sums[1])
}

func assertStatusDone(t *testing.T, done Summary) {
	t.Helper()
	if done.Ticked != 2 || done.Total != 2 || done.Partial != 0 || len(done.Claims) != 0 {
		t.Fatalf("done summary: %+v", done)
	}
}

func assertStatusMain(t *testing.T, main Summary) {
	t.Helper()
	if main.Ticked != 1 || main.Total != 4 || main.Partial != 1 {
		t.Fatalf("main summary: %+v", main)
	}
	if len(main.Claims) != 1 || main.Claims[0] != "1.3: working: alice on it" {
		t.Fatalf("claims: %v", main.Claims)
	}
}

// TestActive picks the newest dated file with an open task, skipping undated and fully ticked ones.
func TestActive(t *testing.T) {
	hermetic(t)
	dir := t.TempDir()
	open := "- [ ] 1.1 Open task owns: a/b.go\n"
	done := "- [x] 1.1 Done task owns: a/b.go (deadbee)\n"
	files := []struct {
		name, body string
		mtime      time.Time
	}{
		{"2026-10-01-old.md", open, time.Unix(1000, 0)},
		{"2026-10-03-new.md", open, time.Unix(3000, 0)},
		{"2026-10-02-done.md", done, time.Unix(4000, 0)},
		{"scratch.md", open, time.Unix(5000, 0)},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		writeFile(t, p, f.body)
		if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
	got, parsed, ok := Active(dir, "", nil)
	if !ok || !strings.HasSuffix(got, "2026-10-03-new.md") {
		t.Fatalf("Active=%q,%v", got, ok)
	}
	if len(parsed.Tasks) != 1 || !strings.Contains(parsed.Tasks[0].Title, "Open task") {
		t.Fatalf("Active returned plan %+v, want the winner's parsed tasks", parsed.Tasks)
	}

	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "2026-10-02-done.md"), done)
	writeFile(t, filepath.Join(empty, "scratch.md"), open)
	if got, _, ok := Active(empty, "", nil); ok || got != "" {
		t.Fatalf("Active on done-only dir=%q,%v", got, ok)
	}
}

// TestRepoPlanTemplate guards docs/templates/plan.md: keeps the "# Plan:" heading and "## Tasks" section.
func TestRepoPlanTemplate(t *testing.T) {
	body, err := os.ReadFile(TemplatePath(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Plan:", "## Tasks"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("repo template missing %q", want)
		}
	}
}

// A symlinked .md (pointing outside plans/) is never summarized.
func TestStatusSkipsSymlink(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
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
	if got, _, ok := Active(dir, "", []string{draft}); ok {
		t.Fatalf("Active picked skipped draft %s", got)
	}
	if got, _, ok := Active(dir, "", nil); !ok || got != draft {
		t.Fatalf("Active without skip = %q, %v", got, ok)
	}
}

// Active's pre-filter keeps every plan the parser finds an open owned task in and drops closed ones.
func TestMayHaveOpenOwned(t *testing.T) {
	cases := map[string]string{
		"wrapped owns":   "- [x] 1.0 Done owns: z\n  * [ ] 1.1 T\n    OWNS: a.go\n",
		"open owned":     "- [ ] 1.1 T owns: a\n",
		"done":           "- [x] 1.1 T owns: a\n",
		"struck":         "- [ ] ~~1.1 T owns: a~~ — dropped\n",
		"open no owns":   "- [ ] 1.1 T\n\nowns: a\n",
		"owns elsewhere": "- [ ] 1.1 T\n- [x] 1.2 U owns: a\n",
	}
	for name, md := range cases {
		want := hasOpenOwnedTask(parseBytes("", []byte(md)))
		if got := mayHaveOpenOwned([]byte(md)); got != want {
			t.Errorf("%s: mayHaveOpenOwned = %v, parser says %v", name, got, want)
		}
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
	if got, _, _ := Active(dir, "fx-1", nil); got != claimed {
		t.Fatalf("Active(fx-1) = %q, want the claimed plan", got)
	}
	if got, _, _ := Active(dir, "other", nil); got != newer {
		t.Fatalf("Active(other) = %q, want the newest plan", got)
	}
}

// Checkbox lines inside fenced code, nested or tilde fences included, are not tasks.
func TestParseSkipsFencedCheckboxes(t *testing.T) {
	cases := map[string]struct{ md, want string }{
		"plain and tilde fences": {
			"- [ ] 1.1 Real owns: a\n\n```md\n- [ ] 1.2 Fake owns: b\n```\n\n~~~\n- [ ] 1.3 Fake\n~~~\n- [ ] 1.4 Real2 owns: c\n",
			"1.1 1.4"},
		"longer fence closed only by an equal-or-longer one": {
			"- [ ] 1.1 Real owns: a.go\n\n````md\n```\n- [ ] 9.9 Sample owns: x.go\n```\n- [ ] 9.8 Sample2 owns: y.go\n````\n\n~~~\n```\n- [ ] 9.7 Tilde owns: z.go\n~~~\n\n- [ ] 2.1 Second owns: b.go\n",
			"1.1 2.1"},
		"gate and verify rows keep their letter": {
			"- [ ] Phase 1.G — gate\n- [ ] Phase 1.V — verify\n- [ ] Phase 1.2a — split\n",
			"Phase 1.G Phase 1.V Phase 1.2a"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var ids []string
			for _, task := range parseBytes("p.md", []byte(c.md)).Tasks {
				ids = append(ids, task.ID)
			}
			if got := strings.Join(ids, " "); got != c.want {
				t.Fatalf("task ids = %q, want %q", got, c.want)
			}
		})
	}
}

// Struck-out tasks (text opens with ~~ and closes later on the line) are dropped: not done, not open, not in the total.
func TestDroppedTasks(t *testing.T) {
	cases := []struct {
		name, md                           string
		wantIDs                            string // every parsed task id, dropped ones included
		wantTicked, wantTotal, wantDropped int
	}{
		{"struck open with superseded note", "- [ ] ~~Phase 2.1 — add the foo verb~~ — superseded: folded into agents sync (a1b2c3d4)\n",
			"Phase 2.1", 0, 0, 1},
		{"ticked and struck, strike wins", "- [x] ~~Phase 2.2 — old thing~~ (deadbee)\n", "Phase 2.2", 0, 0, 1},
		{"tilde only in the middle is open", "- [ ] Phase 1 — use ~~old~~ new name\n", "Phase 1", 0, 1, 0},
		{"unclosed tilde is open", "- [ ] ~~Phase 1.5 — never closed\n", "Phase 1.5", 0, 1, 0},
		{"mixed plan", "- [x] Phase 1.1 a\n- [x] Phase 1.2 b\n- [x] Phase 1.3 c\n" +
			"- [ ] Phase 2.1 d\n- [ ] Phase 2.2 e\n" +
			"- [ ] ~~Phase 3.1 f~~ — superseded (a1b2c3d4)\n- [x] ~~Phase 3.2 g~~\n- [ ] ~~Phase 3.3 h~~\n- [x] ~~Phase 3.4 i~~ (deadbee)\n",
			"Phase 1.1 Phase 1.2 Phase 1.3 Phase 2.1 Phase 2.2 Phase 3.1 Phase 3.2 Phase 3.3 Phase 3.4", 3, 5, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := parseBytes("p.md", []byte(c.md))
			var ids []string
			for _, task := range plan.Tasks {
				ids = append(ids, task.ID)
			}
			if got := strings.Join(ids, " "); got != c.wantIDs {
				t.Fatalf("ids = %q, want %q", got, c.wantIDs)
			}
			s := summarize(plan)
			if s.Ticked != c.wantTicked || s.Total != c.wantTotal || s.Dropped != c.wantDropped {
				t.Fatalf("ticked/total/dropped = %d/%d/%d, want %d/%d/%d",
					s.Ticked, s.Total, s.Dropped, c.wantTicked, c.wantTotal, c.wantDropped)
			}
		})
	}
}

// ClaimFree leaves a live claim alone and ClearClaimOwned clears only its owner's claim.
func TestClaimFreeAndClearOwned(t *testing.T) {
	hermetic(t)
	p := filepath.Join(t.TempDir(), "2026-10-29-x.md")
	writeFile(t, p, "- [ ] 1.2 task owns: a.go\n")
	steps := []struct {
		do         func() error
		has, hasNo string
		wantErr    error
	}{
		{func() error { return ClaimFree(p, "1.2", "working: Agent u1 · since 10:00") }, "u1", "", nil},
		{func() error { return ClaimFree(p, "1.2", "working: Agent u2 · since 10:01") }, "u1", "u2", ErrClaimed},
		{func() error { return ClearClaimOwned(p, "1.2", " u2 ") }, "u1", "", nil},
		{func() error { return ClearClaimOwned(p, "1.2", " u1 ") }, "", "working:", nil},
	}
	for i, s := range steps {
		if err := s.do(); !errors.Is(err, s.wantErr) {
			t.Fatalf("step %d: err = %v, want %v", i, err, s.wantErr)
		}
		b := readFile(t, p)
		if !strings.Contains(b, s.has) || (s.hasNo != "" && strings.Contains(b, s.hasNo)) {
			t.Fatalf("step %d: want %q present and %q absent in:\n%s", i, s.has, s.hasNo, b)
		}
	}
}

// tickRangeRepo builds a repo with a base commit and three file commits; it returns the repo and the base sha.
func tickRangeRepo(t *testing.T) (repo, base string) {
	t.Helper()
	repo = t.TempDir()
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base = strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD"))
	for _, rel := range []string{"internal/a/a.go", "internal/b/b.go", "README.md"} {
		writeFile(t, filepath.Join(repo, rel), "x")
		hosttest.In(t, repo, "add", rel)
		hosttest.In(t, repo, "commit", "-q", "-m", rel+"\n\nTask: plan 1.1\nTask: plan 1.2\nTask: 2026-01-01-old 1.1")
	}
	return repo, base
}

// TestTickRange ticks a plan from real commits naming its tasks: dry-run first, then applied with the first sha.
func TestTickRange(t *testing.T) {
	hermetic(t)
	repo, base := tickRangeRepo(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Solo owns: internal/b/b.go\n- [ ] 1.2 Both owns: internal/a/a.go, internal/b/b.go\n- [ ] 1.3 Other owns: internal/zzz/\n")
	commits, err := rangeCommits(context.Background(), repo, base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 || commits[0].Files[0] != "internal/a/a.go" || len(commits[0].Sha) != 7 {
		t.Fatalf("commits oldest first, 7-char shas: %+v", commits)
	}
	assertTickDryRun(t, plan, commits)
	assertTickApplied(t, plan, commits)
	if _, err := rangeCommits(context.Background(), repo, "HEAD"); err == nil {
		t.Fatal("a range without .. must be rejected")
	}
}

// assertTickDryRun checks the dry-run results and that the plan stays unwritten.
func assertTickDryRun(t *testing.T, plan string, commits []Commit) {
	t.Helper()
	res, err := tickCommits("", plan, commits, true, nil)
	if err != nil || len(res) != 1 || len(res[0].Ticked) != 2 {
		t.Fatalf("dry-run results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); strings.Contains(got, "[x]") || strings.Contains(got, "partly") {
		t.Fatalf("dry-run wrote the plan:\n%s", got)
	}
}

// assertTickApplied ticks for real and checks the written plan.
func assertTickApplied(t *testing.T, plan string, commits []Commit) {
	t.Helper()
	if _, err := tickCommits("", plan, commits, false, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, plan)
	if !strings.Contains(got, "- [x] 1.1 Solo") || !strings.Contains(got, "("+commits[0].Sha+")") || !strings.Contains(got, "- [ ] 1.3") {
		t.Fatalf("applied plan:\n%s", got)
	}
}

// Every commit of a range naming a task is a trailer, but the task ticks once, with the first sha.
func TestTickCommitsTicksNamedTaskOnce(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Split owns: a.go, b.go, c.go\n")
	commits := []Commit{{Sha: "aaa1111", Files: []string{"a.go"}, Message: "feat: x\n\nTask: plan 1.1"}, {Sha: "bbb2222", Files: []string{"b.go"}, Message: "feat: x\n\nTask: plan 1.1"}}
	res, err := tickCommits("", plan, commits, false, nil)
	if err != nil || len(res) != 1 || len(res[0].Ticked) != 1 || res[0].Sha != "aaa1111" {
		t.Fatalf("results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); !strings.Contains(got, "- [x] 1.1") || !strings.Contains(got, "(aaa1111)") {
		t.Fatalf("plan:\n%s", got)
	}
}

// An owns entry abbreviated against the previous one resolves to the spliced path.
func TestCoversSplicesAbbreviatedOwns(t *testing.T) {
	owns := []string{"skills/subagent-workflows/references/prompts.md", "references/cost-routing.md", "references/lane.md"}
	if all, _ := covers("", owns, []string{"skills/subagent-workflows/references/prompts.md", "skills/subagent-workflows/references/cost-routing.md", "skills/subagent-workflows/references/lane.md"}); !all {
		t.Fatal("abbreviated entry not resolved")
	}
	if all, _ := covers("", []string{"a/b.go", "references/x.md"}, []string{"a/b.go", "references/x.md"}); !all {
		t.Fatal("repo-root entry must stay as is")
	}
	// A bare name sits beside the previous entry; a root-level file of that exact name still matches as written.
	if all, _ := covers("", []string{"internal/configure/configure.go", "keysmenu.go"},
		[]string{"internal/configure/configure.go", "internal/configure/keysmenu.go"}); !all {
		t.Fatal("bare name not resolved against the previous entry's directory")
	}
	if all, _ := covers("", []string{"internal/configure/configure.go", "keysmenu.go"},
		[]string{"internal/configure/configure.go", "internal/other/keysmenu.go"}); all {
		t.Fatal("bare name matched a file in another directory")
	}
	if all, _ := covers("", []string{"a/b.go", "Makefile"}, []string{"a/b.go", "Makefile"}); !all {
		t.Fatal("a root file named literally must stay as is")
	}
}

// TestDirOwnsNeverComplete is the regression for a directory owns entry ticking a task on one stray file.
func TestDirOwnsNeverComplete(t *testing.T) {
	if all, any := covers("", []string{"internal/proxy"}, []string{"internal/proxy/x.go"}); all || !any {
		t.Fatalf("dir-only owns: all=%v any=%v, want touched only", all, any)
	}
	if all, _ := covers("", []string{"internal/proxy/", "internal/a.go"}, []string{"internal/a.go"}); all {
		t.Fatal("dir entry untouched must keep the task open")
	}
	if all, _ := covers("", []string{"internal/proxy", "internal/a.go"}, []string{"internal/a.go", "internal/proxy/x.go"}); !all {
		t.Fatal("files plus touched dir should complete")
	}
}

// TestSelectPicksOwningPlan: only the plan whose open task owns a touched file is selected; a dir-only touch is not.
func TestSelectPicksOwningPlan(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-roles.md"), "- [ ] 1.1 Roles owns: internal/roles/r.go\n")
	writeFile(t, filepath.Join(home, "2026-10-01-gateway.md"), "- [ ] 2.1 Proxy owns: internal/proxy\n- [ ] 2.2 Other owns: internal/gw/g.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/roles/r.go", "internal/proxy/h.go"}}}
	plan, tied, _ := Select(home, "", "", nil, commits)
	if len(plan) != 1 || filepath.Base(plan[0]) != "2026-10-01-roles.md" || tied != nil {
		t.Fatalf("plan=%q tied=%v", plan, tied)
	}
}

// TestSelectTieRule: plans owning the same file are all updated and named in tied; a worktree claim keeps one.
func TestSelectTieRule(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-roles.md"), "- [ ] 1.1 Roles owns: internal/roles/r.go\n")
	writeFile(t, filepath.Join(home, "2026-10-01-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/roles/r.go"}}}
	if plan, tied, _ := Select(home, "", "", nil, commits); len(plan) != 2 || len(tied) != 2 {
		t.Fatalf("a tie with no claim must update both: plan=%v tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-01-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go <!-- working: x · worktree wt -->\n")
	if plan, tied, _ := Select(home, "", "wt", nil, commits); len(plan) != 1 || filepath.Base(plan[0]) != "2026-10-01-dup.md" || tied != nil {
		t.Fatalf("worktree claim should break the tie: plan=%v tied=%v", plan, tied)
	}
	day := "2026-10-05"
	writeFile(t, filepath.Join(home, day+"-a.md"), "- [ ] 1.1 A owns: shared.go <!-- working: x · worktree wt -->\n")
	writeFile(t, filepath.Join(home, day+"-b.md"), "- [ ] 2.1 B owns: shared.go\n- [ ] 2.2 U owns: unique.go\n")
	both := []Commit{{Sha: "abc1234", Files: []string{"shared.go", "unique.go"}}}
	plan, _, allow := Select(home, "", "wt", nil, both)
	b := filepath.Join(home, day+"-b.md")
	if !slices.Contains(plan, b) {
		t.Fatalf("independent unique.go task excluded with its entire plan: %v", plan)
	}
	shared, _ := findTask(parseBytes(b, []byte(readFile(t, b))), "2.1")
	unique, _ := findTask(parseBytes(b, []byte(readFile(t, b))), "2.2")
	if allow[b](shared) || !allow[b](unique) {
		t.Fatalf("the claim must hold shared.go only: shared=%v unique=%v", allow[b](shared), allow[b](unique))
	}
}

// TestSelectOlderPlanWhileNewerIsOpen: a commit advances a task in an older plan though a newer plan has open tasks.
func TestSelectOlderPlanWhileNewerIsOpen(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-old.md"), "- [ ] 1.1 Old owns: internal/a/a.go\n")
	writeFile(t, filepath.Join(home, "2026-10-09-new.md"), "- [ ] 2.1 New owns: internal/b/b.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/a/a.go", "internal/b/b.go"}}}
	plan, tied, _ := Select(home, "", "", nil, commits)
	if len(plan) != 2 || tied != nil {
		t.Fatalf("both plans advance, no tie: plan=%v tied=%v", plan, tied)
	}
	if plan, _, _ := Select(home, "", "", nil, []Commit{{Sha: "abc1234", Files: []string{"internal/a/a.go"}}}); len(plan) != 1 || filepath.Base(plan[0]) != "2026-10-01-old.md" {
		t.Fatalf("older plan not selected: %v", plan)
	}
}

// TestStruckTaskIsClosed: a struck task is never selected, ticked or noted, and does not keep a plan active.
func TestStruckTaskIsClosed(t *testing.T) {
	home := hermetic(t)
	dir := filepath.Join(home, "plans")
	struck := filepath.Join(dir, "2026-10-01-struck.md")
	writeFile(t, struck, "- [ ] ~~1.1 Old owns: internal/a/a.go~~ — superseded: gone <!-- working: x · worktree wt -->\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/a/a.go"}}}
	if plan, _, _ := Select(dir, "", "wt", nil, commits); plan != nil {
		t.Fatalf("struck task selected: %v", plan)
	}
	if _, _, ok := Active(dir, "wt", nil); ok {
		t.Fatal("a plan whose only open task is struck must not be active")
	}
	before := readFile(t, struck)
	if ticked, partial := tickOK(t, struck, "abc1234", "internal/a/a.go"); len(ticked)+len(partial) != 0 || readFile(t, struck) != before {
		t.Fatalf("struck task changed: %v %v\n%s", ticked, partial, readFile(t, struck))
	}
	if claimsWorktree(Plan{Tasks: []Task{{Dropped: true, Claim: "working: x · worktree wt"}}}, "wt") {
		t.Fatal("a struck task's claim must not count")
	}
}

func TestClaimsWorktreeMatchesWholeName(t *testing.T) {
	plan := Plan{Tasks: []Task{{Claim: "working: x · worktree fix-2"}}}
	if claimsWorktree(plan, "fix") {
		t.Error("worktree fix matched a claim naming fix-2")
	}
	if !claimsWorktree(plan, "fix-2") {
		t.Error("worktree fix-2 did not match its own claim")
	}
}

func TestSelectAbbreviatedSubsetOwns(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	planPath := filepath.Join(home, "2026-11-05-abbreviated.md")
	writeFile(t, planPath, "- [ ] 1.1 Parser owns: internal/plans/plans.go, tick.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/plans/tick.go"}, Message: "feat: x\n\nTask: 2026-11-05-abbreviated 1.1"}}
	plan, tied, _ := Select(home, "", "", nil, commits)
	if len(plan) != 1 || plan[0] != planPath || len(tied) != 0 {
		t.Fatalf("partial abbreviated ownership: plan=%q tied=%v, want %q", plan, tied, planPath)
	}
	results, err := tickCommits("", plan[0], commits, false, nil)
	if err != nil || len(results) != 1 || len(results[0].Ticked) != 1 || len(results[0].Notes) != 1 || !strings.Contains(results[0].Notes[0], "internal/plans/plans.go") {
		t.Fatalf("a named subset commit ticks and names the missing owned file: results=%+v err=%v", results, err)
	}
}

func TestCoversAbsoluteOwnsMatchRepoRelativeFiles(t *testing.T) {
	files := []string{"internal/plans/tick.go"}
	if all, any := covers("/home/x/repo", []string{"/home/x/repo/internal/plans/tick.go"}, files); !all || !any {
		t.Errorf("absolute file entry: all=%v any=%v, want true true", all, any)
	}
	if _, any := covers("/home/x/repo", []string{"/home/x/repo/internal/plans/"}, files); !any {
		t.Error("absolute dir entry should cover a file under it")
	}
	if _, any := covers("/home/x/repo", []string{"/home/x/repo/internal/plans/other.go"}, files); any {
		t.Error("absolute entry for a different file must not be covered")
	}
	if _, any := covers("/home/x/repo", []string{"/home/x/repo/internal/plans/"}, []string{"plans/x.go"}); any {
		t.Error("absolute dir entry under the root must not cover a same-named top-level dir")
	}
	if _, any := covers("/home/y/wt", []string{"/home/x/repo/internal/plans/"}, files); !any {
		t.Error("absolute entry from another checkout still covers by suffix")
	}
}

func TestCommitFilesPreservesLeadingNewline(t *testing.T) {
	hermetic(t)
	repo, _ := tickRangeRepo(t)
	writeFile(t, filepath.Join(repo, "foo.go"), "unchanged\n")
	hosttest.In(t, repo, "add", "foo.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "ordinary path")
	writeFile(t, filepath.Join(repo, "\nfoo.go"), "changed\n")
	hosttest.In(t, repo, "add", "\nfoo.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "newline path")
	files, err := CommitFiles(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{"\nfoo.go"}) {
		t.Fatalf("CommitFiles = %q, want only the newline-prefixed path", files)
	}
	plan := filepath.Join(t.TempDir(), "plan.md")
	body := "- [ ] 1.1 Ordinary file owns: foo.go\n"
	writeFile(t, plan, body)
	ticked, notes := tickOK(t, plan, "abc1234", files...)
	wantTicks(t, "unchanged ordinary path", ticked, notes, 1, "1.1 Ordinary", 1)
	if got := readFile(t, plan); !strings.HasPrefix(got, "- [x] 1.1 Ordinary file owns: foo.go (abc1234)") {
		t.Fatalf("a named task ticks even when its commit missed the owned file:\n%s", got)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func benchPlan() []byte {
	var b strings.Builder
	b.WriteString("# Plan: bench\n\n## Tasks\n\n")
	for range 200 {
		b.WriteString("- [ ] 1.1 Wire the thing (owns: internal/plans/plans.go, internal/cli/*.go)\n  continuation line one\n  continuation line two\n")
	}
	return []byte(b.String())
}

func BenchmarkParseBytes(b *testing.B) {
	raw := benchPlan()
	b.ReportAllocs()
	for b.Loop() {
		parseBytes("p.md", raw)
	}
}

func namedTestTick(t *testing.T, root, plan, sha string, files []string) (ticked, notes []string, err error) {
	t.Helper()
	data := readFile(t, plan)
	var message strings.Builder
	message.WriteString("test\n")
	for _, task := range parseBytes(plan, []byte(data)).Tasks {
		message.WriteString("\nTask: ")
		message.WriteString(strings.TrimSuffix(filepath.Base(plan), ".md"))
		message.WriteString(" ")
		message.WriteString(task.ID)
	}
	results, err := tickCommits(root, plan, []Commit{{Sha: sha, Files: files, Message: message.String()}}, false, nil)
	for _, r := range results {
		ticked = append(ticked, r.Ticked...)
		notes = append(notes, r.Notes...)
	}
	return ticked, notes, err
}

// Outside a checkout an absolute --plan inside the plans home resolves; one outside it is refused.
func TestResolvePlanOutsideCheckout(t *testing.T) {
	dir := hermetic(t)
	in := filepath.Join(dir, "plans", "repo", "2026-10-05-a.md")
	out := filepath.Join(t.TempDir(), "2026-10-05-b.md")
	writeFile(t, in, "- [ ] 1.1 One\n")
	writeFile(t, out, "- [ ] 1.1 One\n")
	if got, err := ResolvePlan("", in); err != nil || got != in {
		t.Errorf("ResolvePlan in home = %q, %v", got, err)
	}
	if _, err := ResolvePlan("", out); err == nil {
		t.Error("a plan outside the plans home must be refused")
	}
}

func BenchmarkOpenPlans(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := Home("")
	if err := os.MkdirAll(home, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := range 80 {
		name := now().AddDate(0, 0, -i).Format(time.DateOnly) + "-p.md"
		if err := os.WriteFile(filepath.Join(home, name), benchPlan(), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		OpenPlans("", 50)
	}
}

func TestDecisionItemIsAnOwnerTask(t *testing.T) {
	hermetic(t)
	p := filepath.Join(t.TempDir(), "2026-10-05-d.md")
	writeFile(t, p, "## Decisions\n\n- [ ] D2 — Pick\n  a) x  b) y\n  Answer: —\n\n## Tasks\n\n- [ ] Phase 1.1 — A — owns: a.go\n")
	pl, err := Parse(p)
	if err != nil || len(pl.Tasks) != 2 || pl.Tasks[0].ID != "D2" || !ownerStep(pl.Tasks[0]) || judgeable(pl.Tasks[0]) {
		t.Fatalf("D2 must parse as an owner item: %+v %v", pl.Tasks, err)
	}
	if err := MarkDone(p, "D2", ""); err != nil || !strings.Contains(readFile(t, p), "- [x] D2 — Pick") {
		t.Fatalf("MarkDone D2: %v", err)
	}
}
