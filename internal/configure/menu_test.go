package configure

import (
	"bytes"
	"github.com/edotau/claude-code/internal/ui/tui"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// menuTestEnv isolates config/home/PATH so keyTargets' vendor probes never touch a real login CLI or
// the real keyring, and loads the real registry on the subscription provider (no live network calls).
func menuTestEnv(t *testing.T) *providers.Registry {
	t.Helper()
	origExtra := extraSections
	t.Cleanup(func() { extraSections = origExtra })
	extraSections = func(*dialog) ([]tui.Section, []func(*dialog)) { return nil, nil }
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // Orca exports its own codex account home; codex must read unconfigured
	t.Setenv("PATH", t.TempDir())
	vaulttest.Put(t, vault.Pins, providers.PinProfile, "max")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// Copilot/codex/gemini pin env beats the vault pin (providers.Pin); a session running inside one of
	// these agents exports its own model/effort pin into the process env.
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "ANTHROPIC_API_KEY",
		providers.PinCopilotModel, providers.PinCopilotEffort, providers.PinCopilotContext,
		providers.PinCodexModel, providers.PinCodexEffort, providers.PinGeminiModel} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	return reg
}

// TestMenuRowsOrderNoSeparateProviderRow: the top-level menu is claude, then every agent.Names() CLI
// in roster order, then Exit (appended by runMenu, not menuRows); sections are `configure <section>` args — "provider" is
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
	want := []string{"claude", "codex", "antigravity", "opencode", "copilot"}
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

// TestRunMenuDoneLeavesImmediatelyAndChangesNothing: Esc (, which pickMenu reports the same way
// as -1) must run no action and leave the pinned provider untouched.
func TestRunMenuDoneLeavesImmediatelyAndChangesNothing(t *testing.T) {
	reg := menuTestEnv(t)
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	calls := 0
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil // Esc
	})
	cur := providers.Pin(providers.PinProfile)
	rc := runMenu(&dialog{reg: reg, out: discardWriter{}})
	if rc != 0 || calls != 1 {
		t.Fatalf("rc=%d calls=%d, want rc=0 after exactly one picker call", rc, calls)
	}
	if got := providers.Pin(providers.PinProfile); got != cur {
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
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil
	})
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "")
	if calls != 0 {
		t.Fatalf("--check invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== provider ==") || strings.Contains(out.String(), "== keys ==") {
		t.Errorf("--check output must still be the Sections walk, without a keys section:\n%s", out.String())
	}
	_ = rc
	out.Reset()
	_ = Run(Options{In: strings.NewReader(""), Out: &out}, "") // piped: Interactive left false
	if calls != 0 {
		t.Fatalf("a piped run invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== provider ==") || strings.Contains(out.String(), "== keys ==") {
		t.Errorf("piped output must still be the Sections walk, without a keys section:\n%s", out.String())
	}
}

// TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick: the mechanism every claude
// sub-picker row relies on to apply a choice — keepOrPin — must write the pin only when the picked
// name differs from the one already in effect; picking the current backend again must not re-pin it.
//
// This is the closest unit-level proof available for "picking claude pins only on an explicit backend
// pick": runAgentMenu's picker is now the pickAgent seam (see agentmenu_test.go).
func TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	d := &dialog{out: discardWriter{}}
	if !keepOrPin(d, "subscription", "subscription") {
		t.Fatal("keepOrPin(same) = false")
	}
	if got := providers.Pin(providers.PinProfile); got != "" {
		t.Errorf("vault pin written on a same-as-current pick: %q", got)
	}
	if !keepOrPin(d, "subscription", "anthropic") {
		t.Fatal("keepOrPin(different) = false")
	}
	if got := providers.Pin(providers.PinProfile); got != "anthropic" {
		t.Errorf("pin(%s) = %q, want %q after an explicit different pick", providers.PinProfile, got, "anthropic")
	}
}

// TestSaveKeysPartialFailureKeepsFailedStagedOthersDiscarded: a Store failure (here, a multi-line
// value secrets.Store genuinely refuses — no mock) must leave only the failed name staged, with its
// shadowing env still set, while every name that did save is dropped from pending and its env restored.
// Teeth: fixing the bad value and rerunning saveKeys flips it to true with pending left empty.
func TestSaveKeysPartialFailureKeepsFailedStagedOthersDiscarded(t *testing.T) {
	vaulttest.Use(t)
	const bad, good = "CONFIGURE_SAVEKEYS_BAD", "CONFIGURE_SAVEKEYS_GOOD"
	badVal := "line one\nline two" // secrets.Store rejects a multi-line value: a real failure, not a mock
	t.Setenv(bad, "prior-bad")
	t.Setenv(good, "prior-good")
	d := &dialog{out: discardWriter{}, pending: map[string]stagedKey{
		bad:  {val: badVal, prev: "prior-bad", had: true},
		good: {val: "good-value", prev: "prior-good", had: true},
	}}
	_ = os.Setenv(bad, badVal)
	_ = os.Setenv(good, "good-value")

	if ok := saveKeys(d); ok {
		t.Fatal("saveKeys = true, want false on a genuine Store failure")
	}
	if _, staged := d.pending[bad]; !staged {
		t.Error("the failed key must remain staged")
	}
	if _, staged := d.pending[good]; staged {
		t.Error("the succeeded key must be dropped from pending")
	}
	if v, _, ok := secrets.Get(good); !ok || v != "good-value" {
		t.Errorf("good key not stored: %q, ok=%v", v, ok)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if _, found := tx.Get(vault.Secrets, bad); found {
			t.Error("the failed key must not end up in the vault")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(good); got != "prior-good" {
		t.Errorf("succeeded key env restored = %q, want prior-good", got)
	}
	if got := os.Getenv(bad); got != badVal {
		t.Errorf("failed key env should still be shadowed by the staged value, got %q", got)
	}

	// Teeth: fix the bad value and rerun — now both succeed and pending empties out.
	d.pending[bad] = stagedKey{val: "fixed-value", prev: "prior-bad", had: true}
	if ok := saveKeys(d); !ok {
		t.Fatal("saveKeys = false after fixing the bad value, want true")
	}
	if len(d.pending) != 0 {
		t.Errorf("pending = %v, want empty after an all-success save", d.pending)
	}
}

// TestRunMenuExitCodeReflectsSaveKeysFailure: runMenu's exit code must surface a Submit-time Store
// failure (1), not silently report success (0), so a scripted `configure` invocation can detect it.
func TestRunMenuExitCodeReflectsSaveKeysFailure(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.Use(t)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	t.Setenv("CONFIGURE_RUNMENU_BAD", "prior-test-value")
	var d *dialog
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		d.stage("CONFIGURE_RUNMENU_BAD", "two\nlines") // a real Store failure, staged via the real seam
		return -1, nil                                 // Esc: leave through Submit's confirm
	})
	d = &dialog{reg: reg, out: discardWriter{}}
	if rc := runMenu(d); rc != 1 {
		t.Fatalf("runMenu rc = %d, want 1 when a staged key fails to save", rc)
	}

	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		return -1, nil // nothing staged this time
	})
	d2 := &dialog{reg: reg, out: discardWriter{}}
	if rc := runMenu(d2); rc != 0 {
		t.Fatalf("runMenu rc = %d, want 0 with nothing staged", rc)
	}
}

// Failed saves retain the staged key and its shadowing environment after runMenu returns.
func TestRunMenuKeepsFailedKeyStagedAfterReturn(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.Use(t)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	const name = "CONFIGURE_RUNMENU_KEEP_BAD"
	t.Setenv(name, "")
	_ = os.Unsetenv(name)
	var d *dialog
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		d.stage(name, "two\nlines") // secrets.Store genuinely rejects a multi-line value
		return -1, nil
	})
	d = &dialog{reg: reg, out: discardWriter{}}
	if rc := runMenu(d); rc != 1 {
		t.Fatalf("runMenu rc = %d, want 1", rc)
	}
	if _, staged := d.pending[name]; !staged {
		t.Error("failed key dropped from pending after runMenu returned, want it kept")
	}
	if got := os.Getenv(name); got != "two\nlines" {
		t.Error("failed key env after return must retain the staged value")
	}
}
