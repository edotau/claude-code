package plans

import (
	"context"
	"os"
	"os/exec"
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
	return dir
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
func tickOK(t *testing.T, path, sha string, files ...string) (ticked, partial []string) {
	t.Helper()
	ticked, partial, err := Tick(path, sha, files)
	if err != nil {
		t.Fatal(err)
	}
	return ticked, partial
}

// wantBytes fails unless path holds exactly want.
func wantBytes(t *testing.T, path, label, want string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Fatalf("%s bytes:\ngot:\n%s\nwant:\n%s", label, got, want)
	}
}

// wantTicks fails unless exactly nTicked tasks ticked (the first containing tickedSub) and nPartial partly landed.
func wantTicks(t *testing.T, label string, ticked, partial []string, nTicked int, tickedSub string, nPartial int) {
	t.Helper()
	if len(ticked) != nTicked || len(partial) != nPartial || (nTicked > 0 && !strings.Contains(ticked[0], tickedSub)) {
		t.Fatalf("%s: ticked=%v partial=%v", label, ticked, partial)
	}
}

// TestTickSpanning drives a plan through partial ticks, a repeat sha, a full tick and a
// claimed-task tick, asserting the exact file bytes after every step.
func TestTickSpanning(t *testing.T) {
	hermetic(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, path, tickPlan)

	// A commit covering one of three owns (the dir entry) marks the task partly landed.
	ticked, partial := tickOK(t, path, "aaaaaaa", "internal/plans/plans.go")
	wantTicks(t, "step1", ticked, partial, 0, "", 1)
	if !strings.Contains(partial[0], "1.1 Build") {
		t.Fatalf("step1: partial=%v", partial)
	}
	after1 := strings.Replace(tickPlan, "(verb row only)\n", "(verb row only) <!-- partly landed: aaaaaaa -->\n", 1)
	wantBytes(t, path, "step1", after1)

	// A second partial sha (the glob entry) extends the existing comment.
	_, partial = tickOK(t, path, "bbbbbbb", "internal/hooks/safety_a_test.go")
	if len(partial) != 1 {
		t.Fatalf("step2: partial=%v", partial)
	}
	after2 := strings.Replace(after1, "aaaaaaa -->", "aaaaaaa, bbbbbbb -->", 1)
	wantBytes(t, path, "step2", after2)

	assertRepeatShaIsNoop(t, path, after2)

	// A commit covering all owns (dir + glob + noted file) ticks the task; the already
	// ticked task 1.3 is skipped even though the commit touches its file.
	ticked, partial = tickOK(t, path, "fffffff",
		"internal/plans/tick.go",
		"internal/hooks/safety_b_test.go",
		"internal/cli/cli.go",
		"internal/done/z.go",
	)
	wantTicks(t, "full tick", ticked, partial, 1, "1.1 Build", 0)
	after4 := strings.Replace(after2, "- [ ] 1.1 Build", "- [x] 1.1 Build", 1)
	after4 = strings.Replace(after4, "bbbbbbb -->\n", "bbbbbbb --> (fffffff)\n", 1)
	wantBytes(t, path, "step4", after4)

	// Ticking the claimed task removes its working: comment and appends the sha.
	ticked, _ = tickOK(t, path, "ccccccc", "internal/claimed/y.go")
	wantTicks(t, "claim tick", ticked, nil, 1, "1.2 Claimed", 0)
	want := strings.Replace(after4,
		"- [ ] 1.2 Claimed task owns: internal/claimed/y.go <!-- working: alice on it -->\n",
		"- [x] 1.2 Claimed task owns: internal/claimed/y.go (ccccccc)\n", 1)
	wantBytes(t, path, "final", want)
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
	ticked, partial := tickOK(t, path, "aaaaaaa", "internal/plans/tick.go")
	wantTicks(t, "repeat sha", ticked, partial, 0, "", 0)
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
		{"bad verb", "2.1", "thinking: x", "must start with \"working\"", false},
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

// TestActive picks the newest dated file with an open owned task, skipping undated and
// fully ticked files even when their mtimes are newer.
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
	got, ok := Active(dir, "", nil)
	if !ok || !strings.HasSuffix(got, "2026-10-03-new.md") {
		t.Fatalf("Active=%q,%v", got, ok)
	}

	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "2026-10-02-done.md"), done)
	writeFile(t, filepath.Join(empty, "scratch.md"), open)
	if got, ok := Active(empty, "", nil); ok || got != "" {
		t.Fatalf("Active on done-only dir=%q,%v", got, ok)
	}
}

// TestSkeleton reads docs/templates/plan.md whole and refuses a file that lost its "# Plan:" heading.
func TestSkeleton(t *testing.T) {
	const tmpl = "# Plan: <title>\n\n## Tasks\n\n- [ ] 1.1 Do it — owns: x/y.go\n"
	dir := t.TempDir()
	writeFile(t, TemplatePath(dir), tmpl)
	if got, err := Skeleton(dir); err != nil || got != tmpl {
		t.Fatalf("skeleton = %q, %v", got, err)
	}
	writeFile(t, TemplatePath(dir), "# Notes\n")
	if _, err := Skeleton(dir); err == nil || !strings.Contains(err.Error(), "no # Plan: heading") {
		t.Fatalf("err = %v, want the missing-heading error", err)
	}
	if _, err := Skeleton(t.TempDir()); err == nil {
		t.Fatal("want an error for a missing template")
	}
}

// TestRepoPlanTemplate guards the real docs/templates/plan.md: it must keep the "# Plan:" heading and "## Tasks" checklist section.
func TestRepoPlanTemplate(t *testing.T) {
	body, err := Skeleton(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Plan:", "## Tasks"} {
		if !strings.Contains(body, want) {
			t.Fatalf("repo template missing %q", want)
		}
	}
}

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

// Checkbox lines inside fenced code, nested or tilde fences included, are not tasks.
func TestParseSkipsFencedCheckboxes(t *testing.T) {
	cases := map[string]struct{ md, want string }{
		"plain and tilde fences": {
			"- [ ] 1.1 Real owns: a\n\n```md\n- [ ] 1.2 Fake owns: b\n```\n\n~~~\n- [ ] 1.3 Fake\n~~~\n- [ ] 1.4 Real2 owns: c\n",
			"1.1 1.4"},
		"longer fence closed only by an equal-or-longer one": {
			"- [ ] 1.1 Real owns: a.go\n\n````md\n```\n- [ ] 9.9 Sample owns: x.go\n```\n- [ ] 9.8 Sample2 owns: y.go\n````\n\n~~~\n```\n- [ ] 9.7 Tilde owns: z.go\n~~~\n\n- [ ] 2.1 Second owns: b.go\n",
			"1.1 2.1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var ids []string
			for _, task := range ParseBytes("p.md", []byte(c.md)).Tasks {
				ids = append(ids, task.ID)
			}
			if got := strings.Join(ids, " "); got != c.want {
				t.Fatalf("task ids = %q, want %q", got, c.want)
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
	}{
		{func() error { return ClaimFree(p, "1.2", "working: Agent u1 · since 10:00") }, "u1", ""},
		{func() error { return ClaimFree(p, "1.2", "working: Agent u2 · since 10:01") }, "u1", "u2"},
		{func() error { return ClearClaimOwned(p, "1.2", " u2 ") }, "u1", ""},
		{func() error { return ClearClaimOwned(p, "1.2", " u1 ") }, "", "working:"},
	}
	for i, s := range steps {
		if err := s.do(); err != nil {
			t.Fatal(err)
		}
		b := readFile(t, p)
		if !strings.Contains(b, s.has) || (s.hasNo != "" && strings.Contains(b, s.hasNo)) {
			t.Fatalf("step %d: want %q present and %q absent in:\n%s", i, s.has, s.hasNo, b)
		}
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tickRangeRepo builds a repo with a base commit and three file commits; it returns the repo and the base sha.
func tickRangeRepo(t *testing.T) (repo, base string) {
	t.Helper()
	repo = t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base = gitT(t, repo, "rev-parse", "HEAD")
	for _, rel := range []string{"internal/a/a.go", "internal/b/b.go", "README.md"} {
		writeFile(t, filepath.Join(repo, rel), "x")
		gitT(t, repo, "add", rel)
		gitT(t, repo, "commit", "-q", "-m", rel)
	}
	return repo, base
}

// TestTickRange ticks a plan from real commits: a partial commit, a completing one, dry-run, and the unticked base.
func TestTickRange(t *testing.T) {
	hermetic(t)
	repo, base := tickRangeRepo(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Solo owns: internal/b/b.go\n- [ ] 1.2 Both owns: internal/a/a.go, internal/b/b.go\n- [ ] 1.3 Other owns: internal/zzz/\n")
	commits, err := RangeCommits(context.Background(), repo, base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 || commits[0].Files[0] != "internal/a/a.go" || len(commits[0].Sha) != 7 {
		t.Fatalf("commits oldest first, 7-char shas: %+v", commits)
	}
	assertTickDryRun(t, plan, commits)
	assertTickApplied(t, plan, commits)
	if _, err := RangeCommits(context.Background(), repo, "HEAD"); err == nil {
		t.Fatal("a range without .. must be rejected")
	}
}

// assertTickDryRun checks the dry-run results and that the plan stays unwritten.
func assertTickDryRun(t *testing.T, plan string, commits []Commit) {
	t.Helper()
	res, err := TickCommits(plan, commits, true)
	if err != nil || len(res) != 2 || len(res[0].Partial) != 1 || len(res[1].Ticked) != 2 || len(res[1].Partial) != 0 {
		t.Fatalf("dry-run results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); strings.Contains(got, "[x]") || strings.Contains(got, "partly") {
		t.Fatalf("dry-run wrote the plan:\n%s", got)
	}
}

// assertTickApplied ticks for real and checks the written plan.
func assertTickApplied(t *testing.T, plan string, commits []Commit) {
	t.Helper()
	if _, err := TickCommits(plan, commits, false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, plan)
	if !strings.Contains(got, "- [x] 1.1 Solo") || !strings.Contains(got, "("+commits[1].Sha+")") || !strings.Contains(got, "- [ ] 1.3") {
		t.Fatalf("applied plan:\n%s", got)
	}
}

// A task whose owned files land across several commits ticks once the range covers them all.
func TestTickCommitsUnionsRange(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, plan, "- [ ] 1.1 Split owns: a.go, b.go, c.go\n")
	commits := []Commit{{Sha: "aaa1111", Files: []string{"a.go"}}, {Sha: "bbb2222", Files: []string{"b.go"}}, {Sha: "ccc3333", Files: []string{"c.go"}}}
	res, err := TickCommits(plan, commits, false)
	if err != nil || len(res) != 3 || len(res[2].Ticked) != 1 {
		t.Fatalf("results %+v err=%v", res, err)
	}
	if got := readFile(t, plan); !strings.Contains(got, "- [x] 1.1") || !strings.Contains(got, "(ccc3333)") {
		t.Fatalf("plan:\n%s", got)
	}
}

// An owns entry abbreviated against the previous one resolves to the spliced path.
func TestCoversSplicesAbbreviatedOwns(t *testing.T) {
	owns := []string{"skills/subagent-workflows/references/prompts.md", "references/cost-routing.md", "references/lane.md"}
	if all, _ := Covers(owns, []string{"skills/subagent-workflows/references/prompts.md", "skills/subagent-workflows/references/cost-routing.md", "skills/subagent-workflows/references/lane.md"}); !all {
		t.Fatal("abbreviated entry not resolved")
	}
	if all, _ := Covers([]string{"a/b.go", "references/x.md"}, []string{"a/b.go", "references/x.md"}); !all {
		t.Fatal("repo-root entry must stay as is")
	}
	// A bare name sits beside the previous entry; a root-level file of that exact name still matches as written.
	if all, _ := Covers([]string{"internal/configure/configure.go", "keysmenu.go"},
		[]string{"internal/configure/configure.go", "internal/configure/keysmenu.go"}); !all {
		t.Fatal("bare name not resolved against the previous entry's directory")
	}
	if all, _ := Covers([]string{"internal/configure/configure.go", "keysmenu.go"},
		[]string{"internal/configure/configure.go", "internal/other/keysmenu.go"}); all {
		t.Fatal("bare name matched a file in another directory")
	}
	if all, _ := Covers([]string{"a/b.go", "Makefile"}, []string{"a/b.go", "Makefile"}); !all {
		t.Fatal("a root file named literally must stay as is")
	}
}

// TestDirOwnsNeverComplete is the regression for a directory owns entry ticking a task on one stray file.
func TestDirOwnsNeverComplete(t *testing.T) {
	if all, any := Covers([]string{"internal/proxy"}, []string{"internal/proxy/x.go"}); all || !any {
		t.Fatalf("dir-only owns: all=%v any=%v, want touched only", all, any)
	}
	if all, _ := Covers([]string{"internal/proxy/", "internal/a.go"}, []string{"internal/a.go"}); all {
		t.Fatal("dir entry untouched must keep the task open")
	}
	if all, _ := Covers([]string{"internal/proxy", "internal/a.go"}, []string{"internal/a.go", "internal/proxy/x.go"}); !all {
		t.Fatal("files plus touched dir should complete")
	}
}

// TestSelectPicksOwningPlan is the regression for annotating the newest plan instead of the one the range belongs to.
func TestSelectPicksOwningPlan(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-roles.md"), "- [ ] 1.1 Roles owns: internal/roles/r.go\n")
	writeFile(t, filepath.Join(home, "2026-10-01-gateway.md"), "- [ ] 2.1 Proxy owns: internal/proxy\n- [ ] 2.2 Other owns: internal/gw/g.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/roles/r.go", "internal/proxy/h.go"}}}
	plan, tied := Select(home, "", nil, commits)
	if filepath.Base(plan) != "2026-10-01-roles.md" || tied != nil {
		t.Fatalf("plan=%q tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-01-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go\n")
	if plan, tied = Select(home, "", nil, commits); plan != "" || len(tied) != 2 {
		t.Fatalf("equal match must annotate nothing: plan=%q tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-01-dup.md"), "- [ ] 3.1 Roles owns: internal/roles/r.go <!-- working: x · worktree wt -->\n")
	if plan, _ = Select(home, "wt", nil, commits); filepath.Base(plan) != "2026-10-01-dup.md" {
		t.Fatalf("worktree claim should break the tie, got %q", plan)
	}
}

// TestSelectNewerPartialBeatsOlderExact: the newest touched plan wins even when an older plan matches exactly.
func TestSelectNewerPartialBeatsOlderExact(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-old.md"), "- [ ] 1.1 Old owns: internal/a/a.go\n")
	writeFile(t, filepath.Join(home, "2026-10-09-new.md"), "- [ ] 2.1 New owns: internal/a/a.go, internal/b/b.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/a/a.go"}}}
	plan, tied := Select(home, "", nil, commits)
	if filepath.Base(plan) != "2026-10-09-new.md" || tied != nil {
		t.Fatalf("plan=%q tied=%v, want the newer partial-coverage plan over the older exact match", plan, tied)
	}
}

// TestSelectIgnoresStalePlan: an older exact match is left alone while a newer open plan exists, unless it holds a claim.
func TestSelectIgnoresStalePlan(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "2026-10-01-old.md"), "- [ ] 1.1 Old owns: internal/a/a.go\n")
	writeFile(t, filepath.Join(home, "2026-10-09-new.md"), "- [ ] 2.1 New owns: internal/b/b.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/a/a.go"}}}
	if plan, tied := Select(home, "", nil, commits); plan != "" || tied != nil {
		t.Fatalf("stale plan selected: plan=%q tied=%v", plan, tied)
	}
	writeFile(t, filepath.Join(home, "2026-10-01-old.md"), "- [ ] 1.1 Old owns: internal/a/a.go <!-- working: x -->\n")
	if plan, _ := Select(home, "", nil, commits); filepath.Base(plan) != "2026-10-01-old.md" {
		t.Fatalf("claimed plan should be selected, got %q", plan)
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

func TestSelectAbbreviatedPartialOwns(t *testing.T) {
	hermetic(t)
	home := t.TempDir()
	planPath := filepath.Join(home, "2026-11-05-abbreviated.md")
	writeFile(t, planPath, "- [ ] 1.1 Parser owns: internal/plans/plans.go, tick.go\n")
	commits := []Commit{{Sha: "abc1234", Files: []string{"internal/plans/tick.go"}}}
	plan, tied := Select(home, "", nil, commits)
	if plan != planPath || len(tied) != 0 {
		t.Fatalf("partial abbreviated ownership: plan=%q tied=%v, want %q", plan, tied, planPath)
	}
	results, err := TickCommits(plan, commits, false)
	if err != nil || len(results) != 1 || len(results[0].Partial) != 1 || len(results[0].Ticked) != 0 {
		t.Fatalf("partial annotation: results=%+v err=%v", results, err)
	}
}

func TestCoversAbsoluteOwnsMatchRepoRelativeFiles(t *testing.T) {
	files := []string{"internal/plans/tick.go"}
	if all, any := Covers([]string{"/home/x/repo/internal/plans/tick.go"}, files); !all || !any {
		t.Errorf("absolute file entry: all=%v any=%v, want true true", all, any)
	}
	if _, any := Covers([]string{"/home/x/repo/internal/plans/"}, files); !any {
		t.Error("absolute dir entry should cover a file under it")
	}
	if _, any := Covers([]string{"/home/x/repo/internal/plans/other.go"}, files); any {
		t.Error("absolute entry for a different file must not be covered")
	}
}

func TestCommitFilesPreservesLeadingNewline(t *testing.T) {
	hermetic(t)
	repo, _ := tickRangeRepo(t)
	writeFile(t, filepath.Join(repo, "foo.go"), "unchanged\n")
	gitT(t, repo, "add", "foo.go")
	gitT(t, repo, "commit", "-q", "-m", "ordinary path")
	writeFile(t, filepath.Join(repo, "\nfoo.go"), "changed\n")
	gitT(t, repo, "add", "\nfoo.go")
	gitT(t, repo, "commit", "-q", "-m", "newline path")
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
	ticked, partial := tickOK(t, plan, "abc1234", files...)
	wantTicks(t, "unchanged ordinary path", ticked, partial, 0, "", 0)
	wantBytes(t, plan, "unchanged ordinary path", body)
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
