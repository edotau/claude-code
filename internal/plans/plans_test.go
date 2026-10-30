package plans

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestTickSpanning drives a plan through partial ticks, a repeat sha, a full tick and a
// claimed-task tick, asserting the exact file bytes after every step.
func TestTickSpanning(t *testing.T) {
	hermetic(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, path, tickPlan)

	// A commit covering one of three owns (the dir entry) marks the task partly landed.
	ticked, partial, err := Tick(path, "aaaaaaa", []string{"internal/plans/plans.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticked) != 0 || len(partial) != 1 || !strings.Contains(partial[0], "1.1 Build") {
		t.Fatalf("step1: ticked=%v partial=%v", ticked, partial)
	}
	after1 := strings.Replace(tickPlan, "(verb row only)\n", "(verb row only) <!-- partly landed: aaaaaaa -->\n", 1)
	if got := readFile(t, path); got != after1 {
		t.Fatalf("step1 bytes:\n%s", got)
	}

	// A second partial sha (the glob entry) extends the existing comment.
	if _, partial, err = Tick(path, "bbbbbbb", []string{"internal/hooks/safety_a_test.go"}); err != nil {
		t.Fatal(err)
	} else if len(partial) != 1 {
		t.Fatalf("step2: partial=%v", partial)
	}
	after2 := strings.Replace(after1, "aaaaaaa -->", "aaaaaaa, bbbbbbb -->", 1)
	if got := readFile(t, path); got != after2 {
		t.Fatalf("step2 bytes:\n%s", got)
	}

	// A repeat sha neither duplicates nor rewrites: bytes and mtime stay put.
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	ticked, partial, err = Tick(path, "aaaaaaa", []string{"internal/plans/tick.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticked) != 0 || len(partial) != 0 {
		t.Fatalf("repeat sha: ticked=%v partial=%v", ticked, partial)
	}
	if got := readFile(t, path); got != after2 {
		t.Fatalf("repeat sha rewrote bytes:\n%s", got)
	}
	if info, err := os.Stat(path); err != nil || info.ModTime().Unix() != stamp.Unix() {
		t.Fatalf("repeat sha rewrote file: mtime=%v err=%v", info.ModTime(), err)
	}

	// A commit covering all owns (dir + glob + noted file) ticks the task; the already
	// ticked task 1.3 is skipped even though the commit touches its file.
	ticked, partial, err = Tick(path, "fffffff", []string{
		"internal/plans/tick.go",
		"internal/hooks/safety_b_test.go",
		"internal/cli/cli.go",
		"internal/done/z.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticked) != 1 || !strings.Contains(ticked[0], "1.1 Build") || len(partial) != 0 {
		t.Fatalf("full tick: ticked=%v partial=%v", ticked, partial)
	}
	after4 := strings.Replace(after2, "- [ ] 1.1 Build", "- [x] 1.1 Build", 1)
	after4 = strings.Replace(after4, "bbbbbbb -->\n", "bbbbbbb --> (fffffff)\n", 1)
	if got := readFile(t, path); got != after4 {
		t.Fatalf("step4 bytes:\ngot:\n%s\nwant:\n%s", got, after4)
	}

	// Ticking the claimed task removes its working: comment and appends the sha.
	ticked, _, err = Tick(path, "ccccccc", []string{"internal/claimed/y.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ticked) != 1 || !strings.Contains(ticked[0], "1.2 Claimed") {
		t.Fatalf("claim tick: ticked=%v", ticked)
	}
	want := strings.Replace(after4,
		"- [ ] 1.2 Claimed task owns: internal/claimed/y.go <!-- working: alice on it -->\n",
		"- [x] 1.2 Claimed task owns: internal/claimed/y.go (ccccccc)\n", 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("final bytes:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasSuffix(want, "\n") {
		t.Fatal("fixture lost its trailing newline")
	}
}

// TestClaimClearClaim exercises claim by ID and by title prefix, replacement, clearing
// and the rejection cases.
func TestClaimClearClaim(t *testing.T) {
	hermetic(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	const plan = `# Plan: claims

- [ ] 2.1 First task owns: internal/a/x.go
- [ ] 2.2 Second task owns: internal/b/y.go <!-- working: carol -->
`
	writeFile(t, path, plan)

	// By ID, appended at the task's last line.
	if err := Claim(path, "2.1", "working: dave"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, "owns: internal/a/x.go <!-- working: dave -->") {
		t.Fatalf("claim by id:\n%s", got)
	}

	// By title prefix, replacing the existing claim rather than stacking a second one.
	if err := Claim(path, "2.2 Second", "finished: carol done"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	line22 := ""
	for l := range strings.SplitSeq(got, "\n") {
		if strings.Contains(l, "2.2 Second") {
			line22 = l
		}
	}
	if strings.Count(line22, "<!--") != 1 || !strings.HasSuffix(line22, "owns: internal/b/y.go <!-- finished: carol done -->") {
		t.Fatalf("claim replace:\n%s", got)
	}

	// Clearing drops the comment; clearing again leaves bytes untouched.
	if err := ClearClaim(path, "2.2"); err != nil {
		t.Fatal(err)
	}
	if got = readFile(t, path); !strings.Contains(got, "- [ ] 2.2 Second task owns: internal/b/y.go\n") {
		t.Fatalf("clear claim:\n%s", got)
	}
	before := got
	if err := ClearClaim(path, "2.2"); err != nil {
		t.Fatal(err)
	}
	if got = readFile(t, path); got != before {
		t.Fatalf("clear without claim rewrote:\n%s", got)
	}

	for _, tc := range []struct {
		name, taskID, text, wantErr string
		clear                       bool
	}{
		{"comment terminator", "2.1", "working: x --> y", "-->", false},
		{"bad verb", "2.1", "thinking: x", "must start with working: or finished:", false},
		{"unknown id", "9.9", "working: x", `no task "9.9"`, false},
		{"unknown clear", "Nope", "", `no task "Nope"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.clear {
				err = ClearClaim(path, tc.taskID)
			} else {
				err = Claim(path, tc.taskID, tc.text)
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
	done, main := sums[0], sums[1]
	if done.Ticked != 2 || done.Total != 2 || done.Partial != 0 || len(done.Claims) != 0 {
		t.Fatalf("done summary: %+v", done)
	}
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

// TestSkeleton pulls the plan template from a config dir's agents/Plan.md, skipping a
// decoy markdown fence that lacks "# Plan:".
func TestSkeleton(t *testing.T) {
	const agent = `# Plan agent

Prose.

` + "```markdown" + `
# Not the template
decoy body
` + "```" + `

More prose.

` + "```markdown" + `
# Plan: <title>

## Tasks

- [ ] 1.1 Do it — owns: x/y.go
` + "```" + `

Trailer.
`
	const want = "# Plan: <title>\n\n## Tasks\n\n- [ ] 1.1 Do it — owns: x/y.go\n"

	t.Run("decoy skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "agents", "Plan.md"), agent)
		got, err := Skeleton(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("skeleton:\n%q", got)
		}
	})

	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "agents", "Plan.md"), "# Agent\n\n```markdown\n# Not it\n```\n")
		if _, err := Skeleton(dir); err == nil || !strings.Contains(err.Error(), "no ```markdown block") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := Skeleton(t.TempDir()); err == nil {
			t.Fatal("want error for missing agents/Plan.md")
		}
	})
}

// TestRepoPlanTemplate guards the real agents/Plan.md: its markdown block must stay
// parseable and keep the "# Plan:" heading and "## Tasks" checklist section.
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
