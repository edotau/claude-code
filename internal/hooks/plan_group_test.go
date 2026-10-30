package hooks

import (
	"bytes"
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
