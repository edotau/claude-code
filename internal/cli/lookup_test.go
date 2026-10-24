package cli

import (
	"strings"
	"testing"
)

// TestCmdSecretsNoArgsPrintsUsageAndExitsTwo: bare `secrets` (no op) is a usage error, not a silent no-op.
func TestCmdSecretsNoArgsPrintsUsageAndExitsTwo(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdSecrets(nil) })
	if code != 2 {
		t.Fatalf("cmdSecrets(nil) = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "usage: claude-code secrets") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}
