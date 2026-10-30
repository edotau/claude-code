package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/plans"
)

// planGroupEnv is a hermetic config dir + HOME, optionally with an agents/Plan.md template.
func planGroupEnv(t *testing.T, withTemplate bool) (cfg, home string) {
	t.Helper()
	cfg, home = t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	if withTemplate {
		md := "# Plan agent\n\n```markdown\n# Plan: <title>\n\n## Context\n\n## Phases\n\n## Tasks\n\n- [ ] skeleton-body-line\n```\n"
		if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "agents", "Plan.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, home
}

func hookJSON(t *testing.T, m map[string]any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

func runEventOut(t *testing.T, event string, m map[string]any) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	code = RunEvent(event, false, hookJSON(t, m), &o, &e)
	return code, o.String(), e.String()
}

// Skeleton: injected on EnterPlanMode, once per plan on plan-mode prompts, re-armed by a non-plan prompt.
func TestPlanSkeletonInjection(t *testing.T) {
	planGroupEnv(t, true)
	prompt := func(mode string) string {
		_, out, _ := runEventOut(t, "UserPromptSubmit", map[string]any{
			"hook_event_name": "UserPromptSubmit", "session_id": "s1", "permission_mode": mode, "prompt": "hello",
		})
		return out
	}
	_, out, _ := runEventOut(t, "PostToolUse", map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "EnterPlanMode",
	})
	if !strings.Contains(out, "# Plan:") || !strings.Contains(out, "additionalContext") {
		t.Fatalf("EnterPlanMode output = %q, want additionalContext with the skeleton", out)
	}
	if got := prompt("plan"); strings.Contains(got, "# Plan:") {
		t.Fatalf("plan prompt after EnterPlanMode re-injected: %q", got)
	}
	// marker is set by the first plan-mode prompt only after a clear; a default prompt clears it
	if got := prompt("default"); strings.Contains(got, "# Plan:") {
		t.Fatalf("default prompt injected the skeleton: %q", got)
	}
	if got := prompt("plan"); !strings.Contains(got, "# Plan:") {
		t.Fatalf("plan prompt after a default prompt = %q, want the skeleton again", got)
	}
	if got := prompt("plan"); strings.Contains(got, "# Plan:") {
		t.Fatalf("second plan prompt re-injected: %q", got)
	}
}

func TestPlanSkeletonNoTemplateFailsOpen(t *testing.T) {
	planGroupEnv(t, false)
	_, out, _ := runEventOut(t, "PostToolUse", map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s2", "tool_name": "EnterPlanMode",
	})
	if strings.Contains(out, "# Plan:") {
		t.Fatalf("no Plan.md but output = %q", out)
	}
}

// plan-gate: the first template-less write to an undated draft is blocked with the skeleton; docs-placement is silent.
func TestPlanGateBlocksWithSkeleton(t *testing.T) {
	cfg, _ := planGroupEnv(t, true)
	draft := filepath.Join(cfg, "plans", "funny-words.md")
	payload := map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": cfg,
		"tool_input": map[string]string{"file_path": draft, "content": "free-form text\n"},
	}
	code, out, errOut := runEventOut(t, "PreToolUse", payload)
	all := out + errOut
	if !strings.Contains(all, "plan-template: BLOCKED") || !strings.Contains(all, "skeleton-body-line") {
		t.Fatalf("code=%d out=%q err=%q, want BLOCKED with skeleton", code, out, errOut)
	}
	if !strings.Contains(all, "deny") && code != ExitBlock {
		t.Fatalf("not a block: code=%d out=%q", code, out)
	}
	var e bytes.Buffer
	runDirect("docs-placement", hookJSON(t, payload), io.Discard, &e)
	if strings.Contains(e.String(), "plan-template") {
		t.Fatalf("docs-placement still emits plan-template: %q", e.String())
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeRel(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Tick spanning: real commit -> PostToolUse Bash -> full tick + partial note + context -> Stop reminder once.
func TestPlanTickSpanningAndReminder(t *testing.T) {
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "widgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	writeRel(t, repo, "a/one.go", "package a\n")
	writeRel(t, repo, "b/two file.go", "package b\n")
	gitIn(t, repo, "add", "a/one.go", "b/two file.go")
	gitIn(t, repo, "commit", "-q", "-m", "x", "--no-gpg-sign")
	sha := strings.TrimSpace(gitIn(t, repo, "rev-parse", "--short=7", "HEAD"))

	dir := plans.Home(repo)
	planPath := filepath.Join(dir, "2026-10-29-widgets.md")
	writeRel(t, dir, "2026-10-29-widgets.md", "# Plan: w\n\n## Tasks\n\n"+
		"- [ ] 1.1 Full task owns: a/one.go, b/two file.go\n"+
		"- [ ] 1.2 Partial task owns: a/one.go, c/never.go\n")
	t.Setenv("HOME", home)

	tick := func(command, out string) string {
		_, o, _ := runEventOut(t, "PostToolUse", map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "tick1", "tool_name": "Bash", "cwd": t.TempDir(),
			"tool_input":    map[string]string{"command": command},
			"tool_response": map[string]string{"stdout": out},
		})
		return o
	}
	cmd := "git -C ~/widgets commit -m x"

	before, _ := os.ReadFile(planPath)
	// corners: no commit line / unknown sha change nothing
	tick(cmd, "nothing to commit\n")
	tick(cmd, "[main deadbee] x\n 1 file changed\n")
	if after, _ := os.ReadFile(planPath); !bytes.Equal(before, after) {
		t.Fatalf("plan changed on a non-landing commit:\n%s", after)
	}

	out := tick(cmd, "[main "+sha+"] x\n 2 files changed\n")
	got, _ := os.ReadFile(planPath)
	text := string(got)
	if !strings.Contains(text, "- [x] 1.1 Full task owns: a/one.go, b/two file.go ("+sha+")") {
		t.Fatalf("task 1.1 not ticked (space filename should match):\n%s", text)
	}
	if !strings.Contains(text, "- [ ] 1.2") || !strings.Contains(strings.ToLower(text), "partly landed") {
		t.Fatalf("task 1.2 should stay open with a partly-landed note:\n%s", text)
	}
	if !strings.Contains(out, "1.1") || !strings.Contains(out, "1.2") {
		t.Fatalf("additionalContext = %q, want both tasks named", out)
	}

	stop := func() string {
		var o, e bytes.Buffer
		RunArgs("Stop", []string{"--docs-plan-reminder"}, hookJSON(t, map[string]any{"hook_event_name": "Stop", "session_id": "tick1"}), &o, &e)
		return o.String()
	}
	if first := stop(); !strings.Contains(first, "systemMessage") || !strings.Contains(first, "1.2") {
		t.Fatalf("first Stop = %q, want a systemMessage naming 1.2", first)
	}
	if second := stop(); second != "" {
		t.Fatalf("second Stop = %q, want nothing", second)
	}
}

// One Bash call with two commits (one detached, one with a quoted filename) ticks both; a fenced checkbox is untouched.
func TestPlanTickMultiCommitDetachedQuoted(t *testing.T) {
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "gadgets")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	commit := func(rel string) string {
		writeRel(t, repo, rel, "x\n")
		gitIn(t, repo, "add", rel)
		gitIn(t, repo, "commit", "-q", "-m", "x", "--no-gpg-sign")
		return strings.TrimSpace(gitIn(t, repo, "rev-parse", "--short=7", "HEAD"))
	}
	s1 := commit("a/one.go")
	s2 := commit("b/café two.go")

	dir := plans.Home(repo)
	planPath := filepath.Join(dir, "2026-10-29-gadgets.md")
	writeRel(t, dir, "2026-10-29-gadgets.md", "# Plan: g\n\n- [ ] 1.1 First owns: a/one.go\n"+
		"- [ ] 1.2 Second owns: b/café two.go\n\n```md\n- [ ] 1.3 Fenced owns: a/one.go\n```\n")
	_, _, _ = runEventOut(t, "PostToolUse", map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "mc1", "tool_name": "Bash", "cwd": repo,
		"tool_input":    map[string]string{"command": "git commit -m a && git commit -m b"},
		"tool_response": map[string]string{"stdout": "[main " + s1 + "] a\n 1 file changed\n[detached HEAD " + s2 + "] b\n 1 file changed\n"},
	})
	got, _ := os.ReadFile(planPath)
	text := string(got)
	for _, want := range []string{"- [x] 1.1 First owns: a/one.go (" + s1 + ")",
		"- [x] 1.2 Second owns: b/café two.go (" + s2 + ")", "\n- [ ] 1.3 Fenced owns: a/one.go\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

// The Stop reminder drops a partial that a later commit fully ticked.
func TestPlanReminderDropsLaterTicked(t *testing.T) {
	_, _ = planGroupEnv(t, false)
	p := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(p, []byte("- [x] 1.1 Done owns: a (abc1234)\n- [ ] 1.2 Open owns: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordPartials("rem1", "plan", p, []string{"1.1 Done owns: a", "1.2 Open owns: b"}, os.Stderr)
	out := stillPartial(readPartial(t, "rem1"))
	if strings.Contains(out, "1.1") || !strings.Contains(out, "1.2") {
		t.Fatalf("stillPartial = %q, want only 1.2", out)
	}
}

func readPartial(t *testing.T, session string) string {
	t.Helper()
	b, err := os.ReadFile(partialFile(session))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// commitRepoFixture builds a command and the (sha, cwd, want) lookups commitRepoDir must answer for it.
type commitLookup struct{ sha, cwd, want string }

func commitRepoFixture(t *testing.T, root string, worktrees bool) (string, []commitLookup) {
	t.Helper()
	g := func(dir string, args ...string) string {
		return strings.TrimSpace(gitIn(t, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...))
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
