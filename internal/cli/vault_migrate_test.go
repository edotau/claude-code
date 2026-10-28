package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type migrateOut struct {
	Applied  bool            `json:"applied"`
	Backup   string          `json:"backup"`
	NeedsKey bool            `json:"needs_key"`
	Plan     json.RawMessage `json:"plan"`
}

func seedLegacyPins(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "dev")
	vaulttest.Put(t, vault.Pins, "CODEX_PROFILE", "gemini")
}

func runMigrate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return captureOutput(t, func() int { return cmdVaultMigrate(args) })
}

func TestVaultMigrateDryRunWritesNothing(t *testing.T) {
	seedLegacyPins(t)
	code, out, errs := runMigrate(t, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got migrateOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
	if got.Applied || got.Backup != "" || len(got.Plan) == 0 {
		t.Fatalf("dry run: applied=%v backup=%q plan=%s", got.Applied, got.Backup, got.Plan)
	}
	if v := providers.Pin("HARNESS_PROVIDER"); v != "dev" {
		t.Errorf("dry run changed the pin: HARNESS_PROVIDER = %q", v)
	}
}

func TestVaultMigrateApplyBacksUpAndIsIdempotent(t *testing.T) {
	seedLegacyPins(t)
	code, out, errs := runMigrate(t, "--apply", "--format", "json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, errs)
	}
	var got migrateOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
	if !got.Applied || got.Backup == "" {
		t.Fatalf("apply: applied=%v backup=%q", got.Applied, got.Backup)
	}
	if _, err := os.Stat(got.Backup); err != nil {
		t.Errorf("backup missing: %v", err)
	}
	if v := providers.Pin("HARNESS_PROVIDER"); v != "" {
		t.Errorf("legacy pin HARNESS_PROVIDER survived apply: %q", v)
	}
	code, out, _ = runMigrate(t)
	if code != 0 || !strings.Contains(out, "nothing to migrate") {
		t.Errorf("second run: exit %d, out %q; want nothing to migrate", code, out)
	}
}

func TestVaultMigrateRejectsPositionalAndBadFormat(t *testing.T) {
	vaulttest.Use(t)
	if code, _, _ := runMigrate(t, "extra"); code == 0 {
		t.Error("positional arg accepted")
	}
	if code, _, _ := runMigrate(t, "--format", "yaml"); code == 0 {
		t.Error("--format yaml accepted")
	}
}

func TestAgentEnvSetShowClearPin(t *testing.T) {
	profileFixture(t, "")
	run := func(args ...string) (int, string, string) {
		return captureOutput(t, func() int { return cmdAgentEnv("codex", args) })
	}
	if code, _, errs := run("acme"); code != 0 {
		t.Fatalf("set: exit %d: %s", code, errs)
	}
	if got := providers.Pin(providers.PinCodexProfile); got != "acme" {
		t.Fatalf("pin = %q, want acme", got)
	}
	if _, out, _ := run(); !strings.Contains(out, "acme") {
		t.Errorf("show = %q, want it to name acme", out)
	}
	if code, _, errs := run("--clear"); code != 0 {
		t.Fatalf("clear: exit %d: %s", code, errs)
	}
	if got := providers.Pin(providers.PinCodexProfile); got != "" {
		t.Errorf("pin after --clear = %q", got)
	}
}

func TestAgentEnvUnknownProfileLeavesPin(t *testing.T) {
	profileFixture(t, "")
	code, _, errs := captureOutput(t, func() int { return cmdAgentEnv("codex", []string{"nope"}) })
	if code == 0 || !strings.Contains(errs, "unknown profile") {
		t.Fatalf("exit %d, stderr %q; want failure naming an unknown profile", code, errs)
	}
	if got := providers.Pin(providers.PinCodexProfile); got != "" {
		t.Errorf("pin set despite error: %q", got)
	}
}

func TestAgentEnvRejectsClearWithProfileAndPinlessAgent(t *testing.T) {
	profileFixture(t, "")
	if code, _, _ := captureOutput(t, func() int { return cmdAgentEnv("codex", []string{"--clear", "acme"}) }); code == 0 {
		t.Error("--clear with a profile accepted")
	}
	if code, _, _ := captureOutput(t, func() int { return cmdAgentEnv("claude", nil) }); code == 0 {
		t.Error("claude has no pin but env succeeded")
	}
}

func TestAskToolsAndNoToolsConflict(t *testing.T) {
	vaulttest.Use(t)
	code, _, errs := captureOutput(t, func() int { return cmdAsk([]string{"--tools", "--no-tools", "hi"}) })
	if code == 0 || !strings.Contains(errs, "mutually exclusive") {
		t.Fatalf("exit %d, stderr %q; want a mutually-exclusive failure", code, errs)
	}
}

// Negative control: a vault with nothing legacy applies as a no-op, with no backup written.
func TestVaultMigrateApplyOnMigratedVaultChangesNothing(t *testing.T) {
	vaulttest.Use(t)
	code, out, errs := runMigrate(t, "--apply", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got migrateOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
	if got.Backup != "" || strings.Contains(string(got.Plan), "HARNESS_PROVIDER") {
		t.Errorf("clean vault: backup=%q plan=%s; want no backup and no changes", got.Backup, got.Plan)
	}
}
