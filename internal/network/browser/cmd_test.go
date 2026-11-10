package browser

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestNetworkSubcommandsRejectUnknownFormat: an unknown --format is a usage error (2) before any work.
func TestNetworkSubcommandsRejectUnknownFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name string
		fn   func() int
	}{
		{"browser", func() int { return Cmd([]string{"--format", "yaml"}) }},
		{"browser --setup --dry-run", func() int {
			return Cmd([]string{"--setup", "--dry-run", "--format",
				"yaml"})
		}},
	}
	for _, c := range cases {
		code, _, stderr := hosttest.CaptureOutput(t, c.fn)
		if code != 2 {
			t.Errorf("network %s: code = %d, want 2; stderr=%q", c.name, code, stderr)
		}
	}
}

// TestNetworkBrowserSetupOnlyFlags: --browser and --dry-run belong to --setup; alone they are usage errors.
func TestNetworkBrowserSetupOnlyFlags(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"--browser", "chrome"}, {"--dry-run"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
		if code != 2 || !strings.Contains(stderr, "belongs to --setup") {
			t.Errorf("%v: code=%d stderr=%q, want 2 + belongs to --setup", args, code, stderr)
		}
	}
}

// TestNetworkBrowserRejectsPositional: `--setup chrome` (forgotten --browser) must not silently set up the default.
