package safety

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// Hard rule 4 never asks on text a quoted heredoc only writes to a file; a body the same command runs still asks.
func TestHarnessGitIgnoresDataHeredocBodies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	cases := []struct {
		name, cmd string
		want      core.Decision
	}{
		{"brief written to a file", "mkdir -p /tmp/a && cat > /tmp/a/task.md <<'EOF'\nNever git stash|checkout|reset|restore|clean.\nEOF", core.Allow},
		{"tee data", "tee notes.md <<'EOF'\ngit reset --hard\nEOF", core.Allow},
		{"written then run", "cat > f.sh <<'EOF'\ngit reset --hard\nEOF\nbash f.sh", core.Ask},
		{"run on the start line", "cat > f.sh <<'EOF' && sh f.sh\ngit stash\nEOF", core.Ask},
		{"chmod then run", "cat > f <<'EOF'\ngit checkout .\nEOF\nchmod +x f && ./f", core.Ask},
		{"unquoted delimiter", "cat > f.md <<EOF\ngit stash\nEOF", core.Ask},
		{"executed heredoc", "bash <<'EOF'\ngit reset --hard\nEOF", core.Ask},
		{"git after the heredoc", "cat > f.md <<'EOF'\nhi\nEOF\ngit reset --hard", core.Ask},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": home, "tool_input": map[string]string{"command": c.cmd}})
			if got, stderr := safetyScanDecision(t, bytes.NewReader(payload)); got != c.want {
				t.Errorf("got %d want %d (%s)", got, c.want, stderr)
			}
		})
	}
}
