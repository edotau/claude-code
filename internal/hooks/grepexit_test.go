package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestGrepExitNote(t *testing.T) {
	for _, tc := range []struct {
		cmd, errText string
		want         bool
	}{
		{"go list -deps ./x | grep -c 'a/b'", "Exit code 1", true},
		{"rtk git grep -n foo; rtk rg -n x", "Exit code 1\n", true},
		{"git grep x", "Exit code 1", true},
		{"grep x f", "  Error: Exit code 1 \nout", true},
		{"grep x f; A=$(false)", "Exit code 1", false},
		{"grep x <<'EOF'\nfoo\nEOF", "Exit code 1", true},
		{"grep x <<A <<B\nbody1\nA\nbody2\nB", "Exit code 1", true},
		{"grep x <<A <<B\nA\nrm x\nB\nrm y", "Exit code 1", false},
		{"echo \"a <<EOF\"; grep x f\nls", "Exit code 1", false},
		{"echo \"a <<EOF\"\ngrep x f", "Exit code 1", true},
		{"echo $((1<<2)); grep x f", "Exit code 1", true},
		{"grep x <<END-MARK\nls\nEND-MARK", "Exit code 1", true},
		{"grep x <<'A B'\nls\nA B", "Exit code 1", true},
		{"grep x <<\\EOF\nls\nEOF", "Exit code 1", true},
		{"grep x <<EOF\nls\n EOF\nEOF", "Exit code 1", true},
		{"grep x <<EOF\nls\n EOF\nEOF\nls", "Exit code 1", false},
		{"grep x <<-EOF\nls\n\tEOF", "Exit code 1", true},
		{"cat <<<hi; grep x <<EOF\nls\nEOF", "Exit code 1", true},
		{"A=1 rtk rg x", "Exit code 1", true},
		{"grep x f # note", "Exit code 1", true},
		{"grep x f | wc -l", "Exit code 1", false},
		{"grep x f", "Exit code 2", false},
		{"grep x f", "Exit code 10", false},
	} {
		payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": tc.cmd}, "error": tc.errText})
		in := core.ParseInput(strings.NewReader(string(payload)))
		if got := GrepExit(in).Context != ""; got != tc.want {
			t.Errorf("%q %q: note=%v, want %v", tc.cmd, tc.errText, got, tc.want)
		}
	}
}
