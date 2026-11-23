package cli

import (
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestAgentsDashboardFlagsAndTerminal(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-h"}, 0, "claude-code agents dashboard"},
		{[]string{"extra"}, 2, "takes no arguments"},
		{[]string{"--interval", "100ms"}, 2, "at least 500ms"},
		{nil, 1, "needs a terminal"},
	} {
		if c.code == 1 && runtime.GOOS == "windows" {
			continue // Windows prints the tree instead of refusing
		}
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdAgentsDashboard(c.args) })
		if code != c.code || !strings.Contains(stderr, c.want) {
			t.Errorf("dashboard %v: exit %d stderr %q, want %d and %q", c.args, code, stderr, c.code, c.want)
		}
	}
}
