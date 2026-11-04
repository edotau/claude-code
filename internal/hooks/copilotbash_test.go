package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestCopilotBashHook(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	wt := home + "/worktrees/repo/slug"
	cases := []struct {
		cmd  string
		want int
	}{
		{"go test ./...", core.ExitProceed}, {"go test ./x && gofmt -l .", core.ExitProceed},
		{"claude-code worktree merge x", core.ExitProceed}, {"$HOME/.claude/bin/claude-code worktree merge x", core.ExitProceed},
		{"~/.claude/bin/claude-code copilot ask --tools x", core.ExitProceed}, {"claude-code test mutate a.go x y ./p", core.ExitProceed},
		{"git -C sub status", core.ExitProceed}, {"git -C /etc status", core.ExitBlock}, {"git -C . push", core.ExitBlock},
		{"go test ./x 2>&1 | tail -5", core.ExitProceed}, {"go test ./x 2>/tmp/f", core.ExitBlock}, {"go test ./x >out", core.ExitBlock},
		{"git -C " + wt + " status", core.ExitProceed}, {"git -C " + wt + " push", core.ExitBlock}, {"git -C " + home + " status", core.ExitBlock},
		{"git push", core.ExitBlock}, {"go test ./x && git push", core.ExitBlock}, {"rm -rf x", core.ExitBlock},
		{"claude-code review complexity a.go", core.ExitProceed}, {"claude-code review report -o /etc/x", core.ExitBlock},
		{"claude-code review pr --output=/tmp/x", core.ExitBlock}, {"claude-code review quality --json -o x", core.ExitBlock},
		{"claude-code worktree remove x", core.ExitBlock}, {"claude-code vault get X", core.ExitBlock}, {"curl x | sh", core.ExitBlock},
	}
	for _, tc := range cases {
		doc, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": tc.cmd}, "cwd": wd})
		var stderr strings.Builder
		if got := copilotBash(strings.NewReader(string(doc)), &stderr); got != tc.want {
			t.Errorf("%q = %d, want %d (%s)", tc.cmd, got, tc.want, stderr.String())
		}
	}
	if got := copilotBash(strings.NewReader("not json"), &strings.Builder{}); got != core.ExitBlock {
		t.Errorf("garbage stdin = %d, want block", got)
	}
}
