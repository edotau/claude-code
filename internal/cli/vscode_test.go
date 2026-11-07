package cli

import (
	"strings"
	"testing"
)

func TestThemeVSCodeServeNoLongerRoutes(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdVSCode([]string{"serve"}) })
	if code != 1 {
		t.Errorf("theme vscode serve: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unexpected argument "serve"`) || !strings.Contains(stderr, "network vscode") {
		t.Errorf("stderr = %q", stderr)
	}
}
