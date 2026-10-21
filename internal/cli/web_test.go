package cli

import "testing"

// TestWebSubcommandsRejectUnknownFormat: browsers/setup/view all validate --format through
// outfmt.Check before doing any work — an unknown value is a usage error (2).
func TestWebSubcommandsRejectUnknownFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		fn   func() int
	}{
		{"browsers", func() int { return cmdWebBrowsers([]string{"--format", "yaml"}) }},
		{"setup --dry-run", func() int { return cmdWebSetup([]string{"--dry-run", "--format", "yaml"}) }},
		{"view", func() int { return cmdWebView([]string{"agent-id", "--format", "yaml"}) }},
	}
	for _, c := range cases {
		code, _, stderr := captureOutput(t, c.fn)
		if code != 2 {
			t.Errorf("web %s: code = %d, want 2; stderr=%q", c.name, code, stderr)
		}
	}
}
