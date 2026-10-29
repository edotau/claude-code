package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/plans"
)

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
