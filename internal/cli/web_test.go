package cli

import (
	"strings"
	"testing"
)

// TestNetworkSubcommandsRejectUnknownFormat: browsers/setup/view all validate --format through
// helptext.CheckFormat before doing any work — an unknown value is a usage error (2).
func TestNetworkSubcommandsRejectUnknownFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		fn   func() int
	}{
		{"browser", func() int { return cmdNetworkBrowser([]string{"--format", "yaml"}) }},
		{"browser --setup --dry-run", func() int { return cmdNetworkBrowser([]string{"--setup", "--dry-run", "--format", "yaml"}) }},
		{"view", func() int { return cmdNetworkView([]string{"agent-id", "--format", "yaml"}) }},
	}
	for _, c := range cases {
		code, _, stderr := captureOutput(t, c.fn)
		if code != 2 {
			t.Errorf("network %s: code = %d, want 2; stderr=%q", c.name, code, stderr)
		}
	}
}

// TestNetworkBrowserSetupOnlyFlags: --browser and --dry-run belong to --setup; alone they are usage errors.
func TestNetworkBrowserSetupOnlyFlags(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"--browser", "chrome"}, {"--dry-run"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdNetworkBrowser(args) })
		if code != 2 || !strings.Contains(stderr, "belongs to --setup") {
			t.Errorf("%v: code=%d stderr=%q, want 2 + belongs to --setup", args, code, stderr)
		}
	}
}
