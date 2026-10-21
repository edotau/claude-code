package paths

import (
	"path/filepath"
	"testing"
)

func TestClaudeJSONFollowsNonDefaultConfigDirOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct{ env, want string }{
		{"", filepath.Join(home, ".claude.json")},
		{filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")}, // default dir set explicitly
		{filepath.Join(home, "wt"), filepath.Join(home, "wt", ".claude.json")},
	}
	for _, c := range cases {
		t.Setenv("CLAUDE_CONFIG_DIR", c.env)
		if got := ClaudeJSON(); got != c.want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: ClaudeJSON() = %s, want %s", c.env, got, c.want)
		}
	}
}
