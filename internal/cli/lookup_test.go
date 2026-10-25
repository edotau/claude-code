package cli

import (
	"strings"
	"testing"
)

// TestCmdSecretsNoArgsPrintsUsageAndExitsZero checks bare secrets displays help without running an op.
func TestCmdSecretsNoArgsPrintsUsageAndExitsZero(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdSecrets(nil) })
	if code != 0 {
		t.Fatalf("cmdSecrets(nil) = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "usage: claude-code secrets") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}
