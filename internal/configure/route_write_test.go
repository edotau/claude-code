package configure

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

// runRealRoute drives Set up's real route step (picker + pin write) after a verified agent, answering the picker with in.
func runRealRoute(t *testing.T, in string, readOnlyVault bool) string {
	t.Helper()
	reg := menuTestEnv(t)
	putRec(t, "codex", "EFFORT", "high") // a stored record: codex keeps its own login (chatgpt), so picking off is a real write
	if readOnlyVault {
		if err := os.Chmod(vault.Path(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(vault.Path(), 0o600) })
	}
	st, ve := setupState, setupVerify
	t.Cleanup(func() { setupState, setupVerify = st, ve })
	setupState = func(*dialog, string) agentState { return agentState{verified: true} }
	setupVerify = func(context.Context, string) error { return nil }
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out, br: bufio.NewReader(strings.NewReader(in))}
	agentSetUp(d, "codex")
	return out.String()
}

// A pin write that fails must end Set up with "stopped at route", not read as a dismissed picker.
func TestSetUpRouteFailedPinWriteIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	got := runRealRoute(t, "1\n", true)
	if !strings.Contains(got, "set up stopped at route:") || strings.Contains(got, "the route was not changed") {
		t.Errorf("failed write: out = %q, want 'stopped at route' and no 'not changed'", got)
	}
}

// A dismissed picker stays quiet: only "the route was not changed", no error.
func TestSetUpRouteCancelIsQuiet(t *testing.T) {
	got := runRealRoute(t, "\n", false)
	if !strings.Contains(got, "signed in; the route was not changed") || strings.Contains(got, "stopped at") {
		t.Errorf("cancel: out = %q, want only 'route was not changed'", got)
	}
}
