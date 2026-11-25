package hooks

import (
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
		{"grep x f | wc -l", "Exit code 1", false},
		{"grep x f", "Exit code 2", false},
		{"grep x f", "Exit code 10", false},
	} {
		in := core.ParseInput(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":` + jsonStr(tc.cmd) + `},"error":` + jsonStr(tc.errText) + `}`))
		if got := GrepExit(in).Context != ""; got != tc.want {
			t.Errorf("%q %q: note=%v, want %v", tc.cmd, tc.errText, got, tc.want)
		}
	}
}
