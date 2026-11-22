package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/shell"
)

// tickFixture commits a/one.go and "b/two file.go" in a fresh repo with a two-task plan; tick feeds a commit result.
func tickFixture(t *testing.T) (planPath, sha string, tick func(command, out string) string) {
	t.Helper()
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "widgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	hosttest.In(t, repo, "config", "commit.gpgsign", "false")
	hosttest.WriteRel(t, repo, "a/one.go", "package a\n")
	hosttest.WriteRel(t, repo, "b/two file.go", "package b\n")
	hosttest.In(t, repo, "add", "a/one.go", "b/two file.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "x\n\nTask: "+time.Now().Format(time.DateOnly)+"-widgets 1.1\nTask: "+time.Now().Format(time.DateOnly)+"-widgets 1.2\nTask: "+time.Now().Format(time.DateOnly)+"-gadgets 1.1\nTask: "+time.Now().Format(time.DateOnly)+"-gadgets 1.2", "--no-gpg-sign")
	sha = strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "--short=7", "HEAD"))

	dir := plans.Home(repo)
	name := time.Now().Format(time.DateOnly) + "-widgets.md" // recent, so commits may tick it
	planPath = filepath.Join(dir, name)
	hosttest.WriteRel(t, dir, name, "# Plan: w\n\n## Tasks\n\n"+
		"- [ ] 1.1 Full task owns: a/one.go, b/two file.go\n"+
		"- [ ] 1.2 Partial task owns: a/one.go, c/never.go\n")
	t.Setenv("HOME", home)

	tick = func(command, out string) string {
		return runDocs(t, Tick, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "tick1", "tool_name": "Bash", "cwd": t.TempDir(),
			"tool_input":    map[string]string{"command": command},
			"tool_response": map[string]string{"stdout": out},
		})
	}
	return planPath, sha, tick
}

// wantDeferred: ticking is deferred to the detached verifier (which cannot start under go test), so the plan stays
// open while PendingCommit still names the tasks the commit would tick.
func wantDeferred(t *testing.T, planPath, repo, sha string, ids ...string) {
	t.Helper()
	got, _ := os.ReadFile(planPath)
	if strings.Contains(string(got), "[x]") {
		t.Fatalf("the hook ticked before the verifier ran:\n%s", got)
	}
	pending, _, err := plans.PendingCommit(repo, sha, nil)
	var have []string
	for _, r := range pending {
		have = append(have, r.IDs...)
	}
	if err != nil || !slices.Equal(have, ids) {
		t.Fatalf("pending ticks for %s = %v (err %v), want %v", sha, have, err, ids)
	}
}

const tickCmd = "git -C ~/widgets commit -m x"

// Tick spanning: a real commit naming two tasks ticks both and notes the owned file the second missed.
func TestPlanTickSpanning(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	cmd := tickCmd

	before, _ := os.ReadFile(planPath)
	// corners: no commit line / unknown sha change nothing
	tick(cmd, "nothing to commit\n")
	tick(cmd, "[main deadbee] x\n 1 file changed\n")
	if after, _ := os.ReadFile(planPath); !bytes.Equal(before, after) {
		t.Fatalf("plan changed on a non-landing commit:\n%s", after)
	}

	out := tick(cmd, "[main "+sha+"] x\n 2 files changed\n")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
	if !strings.Contains(out, "owns: c/never.go") {
		t.Fatalf("additionalContext = %q, want the missing owned file", out)
	}

}

// `git commit -q` prints no sha: the hook falls back to HEAD, only for a real commit command.
func TestPlanTickQuietCommitFallsBackToHead(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	before, _ := os.ReadFile(planPath)
	tick("git -C ~/widgets log --grep=commit", "")
	tick("git -C ~/widgets commit --dry-run -q -m x", "")
	if after, _ := os.ReadFile(planPath); !bytes.Equal(before, after) {
		t.Fatalf("a non-commit command ticked:\n%s", after)
	}
	tick("git -C ~/widgets commit -q -m x", "")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
}

// rtk's compressed `ok <sha>` output behind `cd ~/dir && rtk git commit` still reaches the tick.
func TestPlanTickRtkCommitShape(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	tick("cd ~/widgets && rtk git add a/one.go && rtk git commit -m x", "ok 2 files changed, 3 insertions(+)\nok "+sha+"\n")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
}

// A quiet commit followed by a command that prints output still ticks its HEAD.
func TestPlanTickQuietCommitWithOtherOutput(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	tick("git -C ~/widgets commit -q -m x && echo done", "done\n")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
}

// Two quiet commits inside the two-minute window: each ticks its own sha, the second is not skipped.
func TestPlanTickTwoQuietCommitsTickOwnShas(t *testing.T) {
	planPath, sha1, tick := tickFixture(t)
	repo := filepath.Join(os.Getenv("HOME"), "widgets")
	tick("git -C ~/widgets commit -q -m x", "")
	late, _ := os.ReadFile(planPath)
	hosttest.WriteRel(t, filepath.Dir(planPath), filepath.Base(planPath), string(late)+"- [ ] 1.3 Late owns: c/never.go\n")
	hosttest.WriteRel(t, repo, "c/never.go", "package c\n")
	hosttest.In(t, repo, "add", "c/never.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "y\n\nTask: "+time.Now().Format(time.DateOnly)+"-widgets 1.3", "--no-gpg-sign")
	sha2 := strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "--short=7", "HEAD"))
	tick("git -C ~/widgets commit -q -m y", "")
	wantDeferred(t, planPath, repo, sha1, "1.1", "1.2")
	wantDeferred(t, planPath, repo, sha2, "1.3")
}

// The verb ticks first, the hook then sees the same commit: the plan does not change again.
func TestPlanTickAfterVerbChangesNothing(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	repo := filepath.Join(os.Getenv("HOME"), "widgets")
	if _, _, err := plans.TickCommit(repo, sha, []string{"a/one.go", "b/two file.go"}, nil); err != nil {
		t.Fatal(err)
	}
	once, _ := os.ReadFile(planPath)
	tick(tickCmd, "[main "+sha+"] x\n 2 files changed\n")
	if twice, _ := os.ReadFile(planPath); !bytes.Equal(once, twice) || !strings.Contains(string(twice), "[x] 1.1") {
		t.Fatalf("hook after verb changed the plan:\n%s\n--\n%s", once, twice)
	}
}

func TestPlanTickRelativeDashCAfterCD(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	cmd := "cd " + os.Getenv("HOME") + " && git -C widgets commit -m x"
	tick(cmd, "[main "+sha+"] x\n 2 files changed\n")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
}

// The worktree commit verb ticks like git commit, alone, behind rtk, and after a cd && chain.
func TestPlanTickWorktreeCommitVerb(t *testing.T) {
	planPath, sha, tick := tickFixture(t)
	cmd := "cd " + filepath.Join(os.Getenv("HOME"), "widgets") + " && rtk claude-code worktree commit -m x a/one.go"
	tick(cmd, "[main "+sha+"] x\n 2 files changed\n")
	wantDeferred(t, planPath, filepath.Join(os.Getenv("HOME"), "widgets"), sha, "1.1", "1.2")
}

// One Bash call with two commits (one detached, one with a quoted filename) ticks both; a fenced checkbox is untouched.
func TestPlanTickMultiCommitDetachedQuoted(t *testing.T) {
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "gadgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	hosttest.In(t, repo, "config", "commit.gpgsign", "false")
	commit := func(rel, task string) string {
		hosttest.WriteRel(t, repo, rel, "x\n")
		hosttest.In(t, repo, "add", rel)
		hosttest.In(t, repo, "commit", "-q", "-m", "x\n\nTask: "+time.Now().Format(time.DateOnly)+"-gadgets "+task, "--no-gpg-sign")
		return strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "--short=7", "HEAD"))
	}
	s1 := commit("a/one.go", "1.1")
	s2 := commit("b/café two.go", "1.2")

	dir := plans.Home(repo)
	name := time.Now().Format(time.DateOnly) + "-gadgets.md"
	planPath := filepath.Join(dir, name)
	hosttest.WriteRel(t, dir, name, "# Plan: g\n\n- [ ] 1.1 First owns: a/one.go\n"+
		"- [ ] 1.2 Second owns: b/café two.go\n\n```md\n- [ ] 1.3 Fenced owns: a/one.go\n```\n")
	runDocs(t, Tick, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "mc1", "tool_name": "Bash", "cwd": repo,
		"tool_input":    map[string]string{"command": "git commit -m a && git commit -m b"},
		"tool_response": map[string]string{"stdout": "[main " + s1 + "] a\n 1 file changed\n[detached HEAD " + s2 + "] b\n 1 file changed\n"},
	})
	wantDeferred(t, planPath, repo, s1, "1.1")
	wantDeferred(t, planPath, repo, s2, "1.2")
	if got, _ := os.ReadFile(planPath); !strings.Contains(string(got), "\n- [ ] 1.3 Fenced owns: a/one.go\n") {
		t.Fatalf("the fenced task changed:\n%s", got)
	}
}

// commitRepoFixture builds a command and the (sha, cwd, want) lookups commitRepoDir must answer for it.
type commitLookup struct{ sha, cwd, want string }

func commitRepoFixture(t *testing.T, root string, worktrees bool) (string, []commitLookup) {
	t.Helper()
	g := func(dir string, args ...string) string {
		return strings.TrimSpace(hosttest.In(t, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...))
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	g(root, "init", "-q", a)
	g(a, "commit", "-q", "--allow-empty", "-m", "base")
	if worktrees {
		g(a, "worktree", "add", "-q", "-b", "wt", b)
	} else {
		g(root, "init", "-q", b)
	}
	g(a, "commit", "-q", "--allow-empty", "-m", "a")
	g(b, "commit", "-q", "--allow-empty", "-m", "b")
	cmd := "git -C " + a + " commit -m a && git -C " + b + " commit -m b"
	shaA, shaB := g(a, "rev-parse", "HEAD"), g(b, "rev-parse", "HEAD")
	lookups := []commitLookup{{shaA, root, a}, {shaB, root, b}}
	if worktrees { // a non-commit -C is no candidate, so cwd wins
		lookups = append(lookups, commitLookup{shaB, b, b})
	}
	return cmd, lookups
}

// Each sha of a chained `git -C A commit && git -C B commit` resolves to the repo (or linked worktree) that made it.
func TestCommitRepoDirTriesEveryDashC(t *testing.T) {
	for _, worktrees := range []bool{false, true} {
		root := t.TempDir()
		cmd, lookups := commitRepoFixture(t, root, worktrees)
		for _, l := range lookups {
			c := cmd
			if l.cwd != root {
				c = "git -C " + filepath.Join(root, "a") + " status && git commit -m b"
			}
			if got := commitRepoDir(context.Background(), l.cwd, c, l.sha); got != l.want {
				t.Errorf("worktrees=%v cwd=%s: commitRepoDir = %q, want %q", worktrees, l.cwd, got, l.want)
			}
		}
	}
}

// claimPlanRepo makes a git repo with one dated plan holding open owned tasks 1.2 and 1.3; returns repo and plan path.
func claimPlanRepo(t *testing.T) (repo, plan string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	hosttest.In(t, repo, "init", "-q", "-b", "main")
	plan = filepath.Join(plans.Home(memory.GitToplevel(repo)), "2026-10-29-x.md")
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan),
		"# P\n\n- [ ] 1.2 wire the claim call point owns: a.go\n- [ ] 1.3 delete dead recall helper owns: b.go\n")
	return repo, plan
}

// claimSpawn runs ClaimPlanTask for an Agent spawn of id with the given brief and description.
func claimSpawn(t *testing.T, repo, id, prompt, desc string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": id, "cwd": repo,
		"tool_input": map[string]any{"prompt": prompt, "description": desc, "model": "sonnet"}})
	if err := ClaimPlanTask(core.ParseInput(bytes.NewReader(b)), id, nil); err != nil {
		t.Fatalf("ClaimPlanTask(%s): %v", id, err)
	}
}

// A Task: brief line claims exactly that task with the agent, id, worktree and time; ClearPlanClaim removes it.
func TestClaimPlanTaskThenClearPlanClaim(t *testing.T) {
	repo, plan := claimPlanRepo(t)
	claimSpawn(t, repo, "toolu_c1", "Objective: x\nTask: 1.2\nOwns: a.go", "")
	data, _ := os.ReadFile(plan)
	if !regexp.MustCompile(`1\.2 wire.*<!-- working: Agent toolu_c1 · worktree main · since \d\d:\d\d -->`).Match(data) || strings.Count(string(data), "working:") != 1 {
		t.Fatalf("claim not written on 1.2 only:\n%s", data)
	}
	if err := ClearPlanClaim(repo, "toolu_c1"); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived ClearPlanClaim:\n%s", data)
	}
}

// No Task: line and no confident description match claims nothing; a unique title match claims.
func TestPlanClaimNeedsConfidentMatch(t *testing.T) {
	repo, plan := claimPlanRepo(t)
	claimSpawn(t, repo, "toolu_n1", "no task line", "do things")
	claimSpawn(t, repo, "toolu_n2", "Task: 9.9", "")
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("unconfident dispatch claimed a task:\n%s", data)
	}
	claimSpawn(t, repo, "toolu_n3", "no task line", "Delete dead recall helper")
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "1.3 delete dead recall helper owns: b.go <!-- working: Agent toolu_n3") {
		t.Fatalf("unique description match not claimed:\n%s", data)
	}
}

func TestPlanTickLaneWaitsForMainFastForward(t *testing.T) {
	t.Run("summary", func(t *testing.T) { testLaneLanding(t, false, false, false) })
	t.Run("quiet", func(t *testing.T) { testLaneLanding(t, true, false, false) })
	t.Run("later command fails", func(t *testing.T) { testLaneLanding(t, false, true, false) })
	t.Run("merge commit", func(t *testing.T) { testLaneLanding(t, false, false, true) })
	t.Run("quiet merge commit", func(t *testing.T) { testLaneLanding(t, true, false, true) })
	t.Run("merge commit then failure", func(t *testing.T) { testLaneLanding(t, false, true, true) })
}

func testLaneLanding(t *testing.T, quiet, failed, mergeCommit bool) {
	plan, _, tick := tickFixture(t)
	repo := filepath.Join(os.Getenv("HOME"), "widgets")
	body := "- [ ] 1.1 Landing owns: a/one.go\n"
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), body)
	lane := filepath.Join(t.TempDir(), "lane with spaces")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "landing", lane)
	hosttest.WriteRel(t, lane, "a/one.go", "package a\n// landed\n")
	hosttest.In(t, lane, "add", "a/one.go")
	slug := strings.TrimSuffix(filepath.Base(plan), ".md")
	summary := hosttest.In(t, lane, "commit", "-m", "feat: landing\n\nTask: "+slug+" 1.1")
	sha := strings.TrimSpace(hosttest.In(t, lane, "rev-parse", "--short=7", "HEAD"))
	tick("git -C "+fmt.Sprintf("%q", lane)+" commit -m trailer", summary)
	before, err := os.ReadFile(plan)
	if err != nil || string(before) != body {
		t.Fatalf("lane ticked: %s, %v", before, err)
	}
	if _, _, err := plans.TickCommit(repo, sha, []string{"a/one.go"}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(plan)
	if string(before) != body {
		t.Fatal("unlanded sha ticked through main")
	}
	args := []string{"merge", "--ff-only", "landing"}
	if mergeCommit {
		args = []string{"merge", "--no-ff", "-m", "merge landing", "landing"}
	}
	if quiet {
		args = append(args, "--quiet")
	}
	out := hosttest.In(t, repo, args...)
	if quiet {
		hosttest.In(t, repo, "pack-refs", "--all")
	}
	note := runDocs(t, Tick, map[string]any{"tool_name": "Bash", "cwd": repo, "tool_input": map[string]string{"command": "git " + strings.Join(args, " ")}, "tool_response": map[string]any{"stdout": out, "is_error": failed}})
	got, _ := os.ReadFile(plan)
	if !strings.Contains(string(got), "[x] 1.1 Landing owns: a/one.go ("+sha+")") || !strings.Contains(note, "ticked") {
		t.Fatalf("landing: %s, %s", got, note)
	}
}

func TestPlanNoopLandingReplay(t *testing.T) {
	plan, _, tick := tickFixture(t)
	repo := filepath.Join(os.Getenv("HOME"), "widgets")
	hosttest.In(t, repo, "switch", "-c", "landing")
	hosttest.WriteRel(t, repo, "a/one.go", "package a\n// landing\n")
	hosttest.In(t, repo, "add", "a/one.go")
	slug := strings.TrimSuffix(filepath.Base(plan), ".md")
	hosttest.In(t, repo, "commit", "-qm", "landing\n\nTask: "+slug+" 1.1")
	hosttest.In(t, repo, "switch", "main")
	hosttest.In(t, repo, "merge", "--ff-only", "landing")
	command := "git -C " + fmt.Sprintf("%q", repo) + " pull --ff-only . main"
	if out := tick("git -C "+fmt.Sprintf("%q", repo)+" merge --ff-only landing", ""); !strings.Contains(out, "ticked") {
		t.Fatalf("initial landing: %s", out)
	}
	if err := plans.Reopen(plan, "1.1"); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "pull", "--ff-only", ".", "main")
	if out := tick(command, "Already up to date."); strings.Contains(out, "ticked") {
		t.Errorf("no-op replay: %s", out)
	}
	if b, err := os.ReadFile(plan); err != nil || strings.Contains(string(b), "[x] 1.1") {
		t.Errorf("reopened task reticked: %s %v", b, err)
	}
	hosttest.In(t, repo, "reset", "--hard", "HEAD~1")
	if out := tick(command, ""); strings.Contains(out, "ticked") {
		t.Errorf("reset: %s", out)
	}
	hosttest.In(t, repo, "merge", "--no-ff", "-m", "merge landing", "landing")
	hosttest.In(t, repo, "commit", "--amend", "--no-edit")
	if out := tick(command, ""); strings.Contains(out, "ticked") || strings.Contains(out, "candidate") {
		t.Errorf("amended merge: %s", out)
	}
}

func sharedCommitCandidates(cwd, cmd string) []string {
	var out []string
	shell.EachGit(cmd, cwd, func(dir, verb string, _ []string) bool {
		if slices.Contains([]string{"commit", "merge", "pull", shell.WorktreeCommit}, verb) && dir != cwd {
			out = append(out, dir)
		}
		return false
	})
	return append(out, cwd)
}

func TestPlanTickShellDifferential(t *testing.T) {
	for _, cmd := range planTickShellInputs(t) {
		a, b := commitCandidates("/repo", cmd), sharedCommitCandidates("/repo", cmd)
		if !slices.Equal(a, b) {
			t.Logf("input=%q candidates=%q/%q", cmd, a, b)
			if cmd != "git -C a -C b commit -m x" && cmd != "git -C ~/widgets log --grep=commit" && cmd != "cd dir && git -C child commit -m x" {
				t.Errorf("unclassified candidate disagreement: %q", cmd)
			}
		}
	}
}

func planTickShellInputs(t *testing.T) []string {
	t.Helper()
	out := []string{
		`git commit -m 'say "hello"'`, `X=1 git commit -m x`, `env X=1 git commit -m x`,
		`true && git commit -m x; git status | cat`, `(git commit -m x)`,
		"cat <<'EOF'\ngit commit -m x\nEOF", `echo $(git commit -m x)`,
		"git \\\ncommit -m x", `# git commit -m x`, `cd dir && git commit -m x`,
		"git\tcommit -m x", `git commit -m 'unfinished`, `echo 'git commit -m x'`,
		`git -C a -C b commit -m x`, `cd dir && git -C child commit -m x`, `git -C dir status; git commit -m x`,
		`claude-code worktree commit -m x a.go`, `git commit -m 'a;b'`,
	}
	files, err := filepath.Glob("*test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

func TestRealCommitShellCases(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{`git commit -m 'say "hello"'`, true}, {`X=1 git commit -m x`, true},
		{`env X=1 git commit -m x`, true}, {`true && git commit -m x; git status | cat`, true},
		{`(git commit -m x)`, true}, {"cat <<'EOF'\ngit commit -m x\nEOF", true},
		{`echo $(git commit -m x)`, true}, {"git \\\ncommit -m x", true},
		{`# git commit -m x`, true}, {`cd dir && git commit -m x`, true},
		{"git\tcommit -m x", true}, {`git commit -m 'unfinished`, true},
		{`echo 'git commit -m x'`, false}, {`git commit --dry-run`, false},
		{`git log --grep=commit`, false}, {`claude-code worktree commit -m x`, false},
	} {
		if got := realCommit(tc.cmd); got != tc.want {
			t.Errorf("realCommit(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestCommitCandidatesCompatibility(t *testing.T) {
	cmd := "git -C a -C b commit -m x"
	if got := commitCandidates("/repo", cmd); !slices.Equal(got, []string{"/repo/a", "/repo"}) {
		t.Fatalf("legacy candidate priority changed: %q", got)
	}
	if got := sharedCommitCandidates("/repo", cmd); !slices.Equal(got, []string{"/repo/a/b", "/repo"}) {
		t.Fatalf("shared candidate semantics changed: %q", got)
	}
}

func BenchmarkPlanTickShell(b *testing.B) {
	cmd := strings.Repeat("git status --short\n", 29) + "git -C dir commit -q -m x"
	b.ReportAllocs()
	for b.Loop() {
		realCommit(cmd)
	}
}

// ClaimPlanTask consumes the lookup read ahead of the cap check: a plan rewritten after it still gets the stale-task write.
func TestClaimPlanTaskUsesPrefetchedLookup(t *testing.T) {
	repo, plan := claimPlanRepo(t)
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": "toolu_p1", "cwd": repo,
		"tool_input": map[string]any{"prompt": "Task: 1.2"}})
	in := core.ParseInput(bytes.NewReader(b))
	pre := PrefetchPlan(in)
	<-pre.done
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), "# P\n\n- [ ] 1.3 delete dead recall helper owns: b.go\n")
	if err := ClaimPlanTask(in, "toolu_p1", pre); err == nil {
		t.Fatal("claim re-read the plan instead of using the prefetched lookup")
	}
}
