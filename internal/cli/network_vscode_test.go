package cli

import (
	"strings"
	"testing"
)

func TestNetworkVSCodeHelpListsFlags(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdNetwork([]string{"vscode", "-h"}) })
	if code != 0 {
		t.Errorf("network vscode -h: exit %d, want 0", code)
	}
	for _, want := range []string{"network vscode", "folder", "format", "funnel", "no-tailscale", "port", "status", "stop"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("network vscode -h lacks %q: %q", want, stderr)
		}
	}
}

func TestThemeVSCodeServeNoLongerRoutes(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdVSCode([]string{"serve"}) })
	if code != 1 {
		t.Errorf("theme vscode serve: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unexpected argument "serve"`) || !strings.Contains(stderr, "network vscode") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNetworkHelpVSCodeRowFitsEightyColumns(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return cmdNetwork(nil) })
	found := false
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "  vscode ") {
			found = true
			if n := len([]rune(l)); n > 80 {
				t.Errorf("vscode row is %d columns: %q", n, l)
			}
		}
	}
	if !found {
		t.Errorf("network help has no vscode row: %q", stderr)
	}
}
