package safety

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// Added suppression lines ask in the main session and block in an agent; untouched ones and prose pass.
func TestSuppressVerdictTable(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.py")
	if err := os.WriteFile(old, []byte("a = 1  # noqa: E501\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edit := func(path, oldS, newS string) map[string]any {
		return map[string]any{"file_path": path, "old_string": oldS, "new_string": newS}
	}
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		agent string
		want  core.Decision
		text  string
	}{
		{"edit adds noqa, main asks", "Edit", edit(old, "x = 1", "x = 1  # noqa: E501 # long url"), "", core.Ask, "long url"},
		{"edit adds noqa, agent blocks", "Edit", edit(old, "x = 1", "x = 1  # noqa: E501"), "a1", core.Block, "UNFIXED"},
		{"main without reason", "Edit", edit(old, "x", "y: int  # type: ignore[attr-defined]"), "", core.Ask, "no reason given"},
		{"kept existing marker passes", "Edit", edit(old, "a = 1  # noqa: E501\nb", "a = 1  # noqa: E501\nc"), "a1", core.Allow, ""},
		{"write nolint blocks", "Write", map[string]any{"file_path": filepath.Join(dir, "a.go"),
			"content": "f() //nolint:errcheck\n"}, "a1", core.Block, "a.go:1"},
		{"write over file keeps marker", "Write", map[string]any{"file_path": old, "content": "a = 1  # noqa: E501\nb = 2\n"},
			"a1", core.Allow, ""},
		{"markdown prose passes", "Write", map[string]any{"file_path": filepath.Join(dir, "n.md"),
			"content": "use # noqa sparingly\n"}, "a1", core.Allow, ""},
	}
	for _, c := range cases {
		doc := map[string]any{"tool_name": c.tool, "cwd": dir, "tool_input": c.input}
		if c.agent != "" {
			doc["agent_id"] = c.agent
		}
		payload, _ := json.Marshal(doc)
		got, msg := safetyScanDecision(t, bytes.NewReader(payload))
		if got != c.want || !strings.Contains(msg, c.text) {
			t.Errorf("%s: got %v %q, want %v containing %q", c.name, got, msg, c.want, c.text)
		}
	}
}
