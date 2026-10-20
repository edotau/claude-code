package cli

import "testing"

// TestCmdWebSearchQueryForms: positional and --query both reach OneShot (no key → 1); both at once is usage (2).
func TestCmdWebSearchQueryForms(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"positional", []string{"foo"}, 1},
		{"positional multi-word", []string{"foo", "bar"}, 1},
		{"flag", []string{"--query", "foo"}, 1},
		{"flag and positional", []string{"--query", "a", "b"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No key anywhere: env empty, no keyring tool on PATH, empty config dir — so no network call.
			t.Setenv("TAVILY_API_KEY", "")
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if got := cmdWebSearch(tc.args); got != tc.want {
				t.Fatalf("cmdWebSearch(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
