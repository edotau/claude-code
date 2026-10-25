package cli

import (
	"strings"
	"testing"
)

func TestConfigureResetRejectsUnknownAgent(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"--reset", "--dry-run", "--agent", "x"})
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown --agent "x"`) {
		t.Errorf("stderr = %q", stderr)
	}
}
