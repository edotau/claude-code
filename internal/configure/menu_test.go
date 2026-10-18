package configure

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// menuTestEnv isolates config/home/PATH so keyTargets' vendor probes never touch a real login CLI or
// the real keyring, and loads the real registry on the subscription provider (no live network calls).
func menuTestEnv(t *testing.T) *providers.Registry {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	return reg
}

// TestMenuRowsOrderNoSeparateProviderRow: the top-level menu is claude, then every vendors.Names() CLI
// in roster order, then Done (appended by runMenu, not menuRows); sections are `configure <section>` args — "provider" is
// folded into the claude row and must not appear again as its own entry.
func TestMenuRowsOrderNoSeparateProviderRow(t *testing.T) {
	reg := menuTestEnv(t)
	rows, actions := menuRows(&dialog{reg: reg, out: discardWriter{}})
	if len(rows) != len(actions) {
		t.Fatalf("rows=%d actions=%d, must match 1:1", len(rows), len(actions))
	}
	var labels []string
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	want := []string{"claude", "codex", "gemini", "opencode", "copilot"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Fatalf("menu rows = %v, want %v", labels, want)
	}
	for _, l := range labels {
		if l == "provider" || l == "Provider" {
			t.Fatalf("a separate provider row must not exist alongside claude: %v", labels)
		}
	}
}

// TestMenuRowsAgentScopeShowsOnlyThatRow: --agent codex narrows the whole menu to the codex row alone.
func TestMenuRowsAgentScopeShowsOnlyThatRow(t *testing.T) {
	reg := menuTestEnv(t)
	rows, actions := menuRows(&dialog{reg: reg, o: Options{Vendor: "codex"}, out: discardWriter{}})
	if len(rows) != 1 || rows[0][0] != "codex" || len(actions) != 1 {
		t.Fatalf("rows = %v, want exactly one row: codex", rows)
	}
}

// TestRunMenuDoneLeavesImmediatelyAndChangesNothing: Done (or Esc, which pickMenu reports the same way
// as -1) must run no action and leave the pinned provider untouched.
func TestRunMenuDoneLeavesImmediatelyAndChangesNothing(t *testing.T) {
	reg := menuTestEnv(t)
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	calls := 0
	pickMenu = func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil // Esc
	}
	cur := providers.Pin(providers.PinProvider)
	rc := runMenu(&dialog{reg: reg, out: discardWriter{}})
	if rc != 0 || calls != 1 {
		t.Fatalf("rc=%d calls=%d, want rc=0 after exactly one picker call", rc, calls)
	}
	if got := providers.Pin(providers.PinProvider); got != cur {
		t.Errorf("provider pin changed: %q -> %q", cur, got)
	}
}

// TestCheckAndPipedNeverOpenTheMenu: --check and a piped run keep the old Sections walk; the top-level
// picker must never be invoked, regardless of how pickMenu would answer.
func TestCheckAndPipedNeverOpenTheMenu(t *testing.T) {
	menuTestEnv(t)
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	calls := 0
	pickMenu = func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil
	}
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "")
	if calls != 0 {
		t.Fatalf("--check invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== keys ==") {
		t.Errorf("--check output must still be the Sections walk:\n%s", out.String())
	}
	_ = rc
	out.Reset()
	_ = Run(Options{In: strings.NewReader(""), Out: &out}, "") // piped: Interactive left false
	if calls != 0 {
		t.Fatalf("a piped run invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== keys ==") {
		t.Errorf("piped output must still be the Sections walk:\n%s", out.String())
	}
}

// TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick: the mechanism every claude
// sub-picker row relies on to apply a choice — keepOrPin — must write the pin only when the picked
// name differs from the one already in effect; picking the current backend again must not re-pin it.
//
// This is the closest unit-level proof available for "picking claude pins only on an explicit backend
// pick": pickClaudeBackend's own picker (internal/configure/anthropic.go:180) calls tui.PickBack
// directly rather than through an injectable seam like pickMenu/pickSlots/pickModels, so a fake picker
// cannot drive it to an explicit row choice from outside the tui package (tui.Open requires a real
// stdin TTY and fails fast otherwise). See the report's Unresolved section.
func TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "") // Pin() checks the env var before the file; it must stay unset here
	d := &dialog{out: discardWriter{}}
	if !keepOrPin(d, "subscription", "subscription") {
		t.Fatal("keepOrPin(same) = false")
	}
	if got := providers.Pin(providers.PinProvider); got != "" {
		t.Errorf("pin file written on a same-as-current pick: %q", got)
	}
	if !keepOrPin(d, "subscription", "anthropic") {
		t.Fatal("keepOrPin(different) = false")
	}
	if got := providers.Pin(providers.PinProvider); got != "anthropic" {
		t.Errorf("pin(%s) = %q, want %q after an explicit different pick", providers.PinProvider, got, "anthropic")
	}
}
