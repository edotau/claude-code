package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/router"
)

// TestRouterStartHelpDoesNotStart: `router start -h` printed nothing and spawned the daemon (seen 2026-10-23 when
// an opencode audit leg asked for help). The flag must print usage and leave the router alone.
func TestRouterStartHelpDoesNotStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"start", "-h"}, {"stop", "--help"}, {"status", "-h"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdRouter(args) })
		if code != 1 || !strings.Contains(stderr, "usage: claude-code router") {
			t.Errorf("router %v: want usage + exit 1, got exit %d stderr %q", args, code, stderr)
		}
	}
	if _, ok := router.Running(context.Background()); ok {
		t.Fatal("router was started by a help request")
	}
}

// TestRouterStrayArgumentIsAUsageError: a typo after start/stop/status fails loudly instead of being ignored.
func TestRouterStrayArgumentIsAUsageError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdRouter([]string{"stop", "now"}) })
	if code == 0 || !strings.Contains(stderr, `unexpected argument "now"`) {
		t.Fatalf("want a usage error naming the stray argument, got exit %d stderr %q", code, stderr)
	}
}

// TestSecretsOpHelpPrintsUsage: the same ignored-flag twin in `secrets status|scan|doctor -h` (ISSUE sweep of the
// router fix); each must print usage instead of running the op.
func TestSecretsOpHelpPrintsUsage(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"status", "-h"}, {"scan", "--help"}, {"doctor", "-h"}} {
		code, stdout, stderr := captureOutput(t, func() int { return cmdSecrets(args) })
		if code != 2 || !strings.Contains(stderr, "usage: claude-code secrets") || stdout != "" {
			t.Errorf("secrets %v: want usage + exit 2 and no op output, got exit %d stdout %q", args, code, stdout)
		}
	}
}
