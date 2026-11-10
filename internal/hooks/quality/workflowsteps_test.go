package quality

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realScript reads a saved workflow from this repo's workflows/ (the hook's real input).
func realScript(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return src, dir
}

// stubRoute makes every external stage routable except agent "codex" on profile "nope".
func stubRoute(t *testing.T) {
	orig := stepRoute
	stepRoute = func(who, _, profile, _ string) error {
		if who == "codex" && profile == "nope" {
			return errors.New("profile nope is not in the registry")
		}
		return nil
	}
	t.Cleanup(func() { stepRoute = orig })
}

func TestCheckWorkflowSteps(t *testing.T) {
	stubRoute(t)
	cases := []struct {
		script string
		args   any
		want   []string // substrings, one per expected problem; empty = the call may run
	}{
		{"code-workers.js", map[string]any{"steps": map[string]any{"fix": map[string]any{"effort": "huge"}}}, []string{"steps.fix.effort must be"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"nope": map[string]any{}}}, []string{"steps.nope: unknown stage (allowed: dedupe|find|fix|gate|rederive|review|simplify|test)"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"model": "gpt-5", "agent": "bard"}}}, []string{"steps.find.agent must be"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"model": "gpt-5"}}}, []string{"steps.find.model must be haiku|sonnet|opus|fable"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"find": map[string]any{"profile": "openai:x", "agent": "codex"}}}, []string{"must be a plain name"}},
		{"code-workers.js", `{"steps":{"review":{"agent":"codex","profile":"nope"}}}`, []string{"steps.review: codex has no route: profile nope"}},
		{"code-workers.js", `{"steps":{"bogus":{}}`, []string{"args must be a JSON object"}},
		{"code-workers.js", map[string]any{"profile": "nope", "steps": map[string]any{"review": map[string]any{"agent": "codex"}}}, []string{"codex has no route"}},
		{"code-engineer.js", map[string]any{"steps": map[string]any{"child": map[string]any{"bogus": map[string]any{}}, "size": map[string]any{"profile": "qa"}}},
			[]string{"steps.child.bogus: unknown stage", "steps.size: claude stages run in-context"}},
		{"code-workers.js", map[string]any{"steps": map[string]any{"fix": map[string]any{"model": "opus", "effort": "high"}, "review": map[string]any{"agent": "codex"}}}, nil},
		{"code-engineer.js", map[string]any{"steps": map[string]any{"impl": map[string]any{"model": "opus"}, "child": map[string]any{"fix": map[string]any{"effort": "high"}}}}, nil},
		{"green-keeper-loop.js", nil, nil},
	}
	for _, c := range cases {
		src, dir := realScript(t, c.script)
		got := checkWorkflowSteps(src, dir, c.args)
		if len(got) != len(c.want) {
			t.Errorf("%s %v: got %d problem(s) %q, want %d %q", c.script, c.args, len(got), got, len(c.want), c.want)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s %v: problem %d = %q, want it to contain %q", c.script, c.args, i, got[i], w)
			}
		}
	}
}
