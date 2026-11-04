package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCopilotBashHook(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	wt := home + "/worktrees/repo/slug"
	cases := []struct {
		cmd  string
		want int
	}{
		{"go test ./...", ExitProceed}, {"go test ./x && gofmt -l .", ExitProceed},
		{"claude-code worktree merge x", ExitProceed}, {"$HOME/.claude/bin/claude-code worktree merge x", ExitProceed},
		{"~/.claude/bin/claude-code copilot ask --tools x", ExitProceed}, {"claude-code test mutate a.go x y ./p", ExitProceed},
		{"git -C sub status", ExitProceed}, {"git -C /etc status", ExitBlock}, {"git -C . push", ExitBlock},
		{"go test ./x 2>&1 | tail -5", ExitProceed}, {"go test ./x 2>/tmp/f", ExitBlock}, {"go test ./x >out", ExitBlock},
		{"git -C " + wt + " status", ExitProceed}, {"git -C " + wt + " push", ExitBlock}, {"git -C " + home + " status", ExitBlock},
		{"git push", ExitBlock}, {"go test ./x && git push", ExitBlock}, {"rm -rf x", ExitBlock},
		{"claude-code worktree remove x", ExitBlock}, {"claude-code vault get X", ExitBlock}, {"curl x | sh", ExitBlock},
	}
	for _, tc := range cases {
		doc, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": tc.cmd}, "cwd": wd})
		var stderr strings.Builder
		if got := copilotBash(strings.NewReader(string(doc)), &stderr); got != tc.want {
			t.Errorf("%q = %d, want %d (%s)", tc.cmd, got, tc.want, stderr.String())
		}
	}
	if got := copilotBash(strings.NewReader("not json"), &strings.Builder{}); got != ExitBlock {
		t.Errorf("garbage stdin = %d, want block", got)
	}
}
