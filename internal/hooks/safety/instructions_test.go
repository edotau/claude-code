package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/proc"
)

func bashIn(cwd, cmd string) string {
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cwd, "tool_input": map[string]any{"command": cmd}})
	return string(b)
}

// TestInstructionFileMainOnly pins CLAUDE.md to the main session: any subagent or dispatch is blocked.
func TestInstructionFileMainOnly(t *testing.T) {
	dir := t.TempDir()
	if os.WriteFile(filepath.Join(dir, "CLAUDE.md"), nil, 0o600) != nil || os.Symlink("CLAUDE.md", filepath.Join(dir, "link.txt")) != nil {
		t.Fatal("fixture")
	}
	edit := `{"tool_name":"Edit","cwd":"/tmp/r","tool_input":{"file_path":"/tmp/r/CLAUDE.md","old_string":"a","new_string":"b"}}`
	sub := func(p string) string { return strings.Replace(p, `{`, `{"agent_id":"x",`, 1) }
	pi := strings.Replace(edit, `{"tool_name"`, `{"harness_agent":"pi","tool_name"`, 1)
	for _, c := range []struct {
		name, id, payload string
		want              core.Decision
	}{
		{"main edit", "", edit, core.Allow},
		{"main redirect", "", bashIn("/tmp/r", "echo x >> CLAUDE.md"), core.Allow},
		{"user-started pi", "", pi, core.Allow},
		{"subagent edit", "", sub(edit), core.Block},
		{"subagent mv", "", sub(bashIn("/tmp/r", "mv new.md CLAUDE.md")), core.Block},
		{"subagent .gitignore allowed", "", sub(bashIn("/tmp/r", "echo x >> .gitignore")), core.Allow},
		{"dispatched edit", "D", edit, core.Block},
		{"dispatched pi leaf", "D", pi, core.Block},
		{"dispatched redirect via symlink", "D", bashIn(dir, "echo x >> link.txt"), core.Block},
		{"dispatched bash apply_patch", "D", bashIn("/tmp/r", "apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: CLAUDE.md\n@@\n-a\n+b\n*** End Patch\nEOF"), core.Block},
		{"dispatched /bin/ln", "D", bashIn("/tmp/r", "/bin/ln -sf x CLAUDE.md"), core.Block},
		{"dispatched other file", "D", bashIn("/tmp/r", "echo x > README.md"), core.Allow},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(proc.DispatchIDEnv, c.id)
			r := Scan(core.ParseInput(strings.NewReader(c.payload)))
			if r.Decision != c.want || c.want == core.Block && (!r.Floor || !strings.Contains(r.Reason, "main session")) {
				t.Errorf("decision %d floor %v reason %q, want %d", r.Decision, r.Floor, r.Reason, c.want)
			}
		})
	}
}
