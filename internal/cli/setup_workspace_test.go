package cli

import "testing"

// Usage and arity errors exit 2 before any filesystem work; a bad root exits 1 from the engine.
func TestSetupWorkspaceUsageExits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"bogus"}, 2},
		{[]string{"link", "agents"}, 2},
		{[]string{"init", "a", "b"}, 2},
		{[]string{"init", "--with", "x"}, 2},
		{[]string{"status", t.TempDir()}, 1}, // not a git toplevel
	}
	for _, c := range cases {
		if got := cmdSetupWorkspace(c.args); got != c.want {
			t.Errorf("cmdSetupWorkspace(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}
