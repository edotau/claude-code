package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func selectTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := load(t)
	for _, key := range append([]string{PinProfile, PinModel}, slotPinKeys()...) {
		t.Setenv(key, "")
	}
	return reg
}

func slotPinKeys() []string {
	keys := make([]string, 0, len(models.Slots))
	for _, slot := range models.Slots {
		keys = append(keys, SlotPin(slot))
	}
	return keys
}

func writeProviderPins(t *testing.T, pins map[string]string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error {
		for key := range tx.All(vault.Pins) {
			if err := tx.Put(vault.Pins, key, ""); err != nil {
				return err
			}
		}
		for key, value := range pins {
			if err := tx.Put(vault.Pins, key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectReadsVaultPinsWithSlotPrecedence(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProfile:            "gateway",
		PinModel:              "all-from-vault",
		SlotPin(models.Opus):  "openai:gpt-5-nano",
		SlotPin(models.Haiku): "haiku-from-vault",
	})

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gateway" {
		t.Fatalf("provider = %q, want gateway from vault pins", sel.Provider.Name)
	}
	for _, slot := range models.Slots {
		want := Target{Provider: sel.Provider, Model: "all-from-vault"}
		switch slot {
		case models.Opus:
			want = Target{Provider: reg.Providers["openai"], Model: "gpt-5-nano"}
		case models.Haiku:
			want.Model = "haiku-from-vault"
		}
		got, ok := sel.Slots[slot]
		if !ok || got.Provider != want.Provider || got.Model != want.Model {
			t.Errorf("slot %s = %+v, present %v; want %+v", slot, got, ok, want)
		}
	}
}

func TestSelectIgnoresEnvAndFlagBeatsPin(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProfile:           "gateway",
		PinModel:             "all-from-vault",
		SlotPin(models.Opus): "opus-from-vault",
	})
	t.Setenv(PinProfile, "  gw  ")
	t.Setenv(PinModel, "  all-from-env  ")
	t.Setenv(SlotPin(models.Opus), "env-opus")

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gateway" {
		t.Fatalf("provider = %q, want the vault pin gateway (pins are vault-only)", sel.Provider.Name)
	}
	if got := sel.Slots[models.Opus]; got.Provider != sel.Provider || got.Model != "opus-from-vault" {
		t.Errorf("opus = %+v, want the vault value, not the env", got)
	}
	if got := sel.Slots[models.Sonnet]; got.Provider != sel.Provider || got.Model != "all-from-vault" {
		t.Errorf("sonnet = %+v, want the vault all-model pin, not the env", got)
	}

	t.Setenv(PinProfile, "")
	writeProviderPins(t, map[string]string{
		PinProfile:           "gw",
		SlotPin(models.Opus): "opus-from-vault",
	})
	flagSel, err := Select(reg, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if flagSel.Provider.Name != "gateway" {
		t.Errorf("provider flag selected %q, want gateway over vault pin gw", flagSel.Provider.Name)
	}
	if got := flagSel.Slots[models.Opus]; got.Provider != flagSel.Provider || got.Model != "opus-from-vault" {
		t.Errorf("flag-selected opus = %+v, want vault value on gateway", got)
	}
}

func TestSelectAllModelPinBeatsProviderDefaults(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProfile: "gateway",
		PinModel:   "all-from-vault",
	})

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range models.Slots {
		if got := sel.Slots[slot]; got.Provider != sel.Provider || got.Model != "all-from-vault" {
			t.Errorf("slot %s = %+v, want all-model pin", slot, got)
		}
	}

	writeProviderPins(t, map[string]string{PinProfile: "gateway"})
	sel, err = Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range models.Slots {
		want := sel.Provider.Models[slot]
		got, ok := sel.Slots[slot]
		if want == "" {
			if ok {
				t.Errorf("slot %s = %+v, want no provider default", slot, got)
			}
			continue
		}
		if !ok || got.Provider != sel.Provider || got.Model != want {
			t.Errorf("slot %s = %+v, want provider default %q", slot, got, want)
		}
	}
}

// EffectiveSecret must try env, then the vault, in that order, and miss cleanly.
func TestEffectiveSecretOrder(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("ORDER_NAME", "")
	vaulttest.Secret(t, "ORDER_NAME", "from-vault")

	if v, src, err := EffectiveSecret("ORDER_NAME"); err != nil || v != "from-vault" || src != "vault" {
		t.Errorf("EffectiveSecret (vault) = (%q, %q, %v), want (from-vault, vault, nil)", v, src, err)
	}

	t.Setenv("ORDER_NAME", "  from-env\n")
	if v, src, err := EffectiveSecret("ORDER_NAME"); err != nil || v != "from-env" || src != "env" {
		t.Errorf("EffectiveSecret (env beats vault) = (%q, %q, %v), want (from-env, env, nil)", v, src, err)
	}
}

// A whitespace-only env var must be treated as unset, not as a winning value.
func TestEffectiveSecretIgnoresWhitespaceOnlyEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "WS_NAME", "from-vault")
	t.Setenv("WS_NAME", "   ")

	if v, src, err := EffectiveSecret("WS_NAME"); err != nil || v != "from-vault" || src != "vault" {
		t.Errorf("EffectiveSecret with whitespace-only env = (%q, %q, %v), want (from-vault, vault, nil)", v, src, err)
	}
	if got := HasSecret("WS_NAME"); got != "vault" {
		t.Errorf("HasSecret with whitespace-only env = %q, want vault", got)
	}
}

// A name held nowhere must miss cleanly: ("", "", nil), and HasSecret reports "".
func TestEffectiveSecretMiss(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("NEVER_SET_ANYWHERE", "")

	if v, src, err := EffectiveSecret("NEVER_SET_ANYWHERE"); v != "" || src != "" || err != nil {
		t.Errorf("EffectiveSecret(miss) = (%q, %q, %v), want (\"\", \"\", nil)", v, src, err)
	}
	if got := HasSecret("NEVER_SET_ANYWHERE"); got != "" {
		t.Errorf("HasSecret(miss) = %q, want \"\"", got)
	}
}

// A vault record that cannot be opened surfaces as EffectiveSecret's error; StoredSecret reads it as a miss.
func TestEffectiveSecretLockedVaultErrors(t *testing.T) {
	lockVault(t, "LOCKED_NAME", "sealed")
	t.Setenv("LOCKED_NAME", "")
	if v, src, err := EffectiveSecret("LOCKED_NAME"); err == nil || v != "" || src != "" {
		t.Errorf("EffectiveSecret(locked) = (%q, %q, %v), want an error and no value", v, src, err)
	}
	if got := StoredSecret("LOCKED_NAME"); got != "" {
		t.Errorf("StoredSecret(locked) = %q, want \"\"", got)
	}
}

// HasSecret reports env|vault|"" from presence alone: it never resolves a key, so a wrong key, a
// down keyring and no keyring at all change nothing.
func TestHasSecretNeverResolvesAKey(t *testing.T) {
	lockVault(t, "HAS_NAME", "sealed")
	t.Setenv("HAS_NAME", "")
	t.Setenv("HAS_NOT", "")
	kr := vaulttest.Keyring(t)
	kr.Down = true
	if got := HasSecret("HAS_NAME"); got != "vault" {
		t.Errorf("HasSecret(sealed, wrong key, keyring down) = %q, want vault", got)
	}
	t.Setenv("HARNESS_VAULT_KEY", "")
	if got := HasSecret("HAS_NAME"); got != "vault" {
		t.Errorf("HasSecret(sealed, no key) = %q, want vault", got)
	}
	if got := HasSecret("HAS_NOT"); got != "" {
		t.Errorf("HasSecret(absent) = %q, want \"\"", got)
	}
	t.Setenv("HAS_NAME", "from-env")
	if got := HasSecret("HAS_NAME"); got != "env" {
		t.Errorf("HasSecret(env set) = %q, want env", got)
	}
}

// CopilotModel resolves the COPILOT_MODEL pin first; with no pin, it falls back to the
// github-copilot provider's own opus slot — and names which source won in both cases.
func TestCopilotModelPinBeatsFallback(t *testing.T) {
	reg := load(t)
	t.Setenv(PinCopilotModel, "")
	if err := SetPins(map[string]string{PinCopilotModel: ""}); err != nil {
		t.Fatal(err)
	}

	wantFallback := reg.Providers["personal"].Models[models.Opus]
	if got, src := CopilotModel(reg); got != wantFallback || src != "github-copilot default" {
		t.Errorf("CopilotModel(unset) = (%q, %q), want (%q, github-copilot default)", got, src, wantFallback)
	}

	if err := SetPins(map[string]string{PinCopilotModel: "claude-opus-5.5"}); err != nil {
		t.Fatal(err)
	}
	if got, src := CopilotModel(reg); got != "claude-opus-5.5" || src != "pin" {
		t.Errorf("CopilotModel(pinned) = (%q, %q), want (claude-opus-5.5, pin)", got, src)
	}
}

// A process env var for COPILOT_MODEL is ignored: pins are vault-only.
func TestCopilotModelIgnoresEnv(t *testing.T) {
	reg := load(t)
	if err := SetPins(map[string]string{PinCopilotModel: "from-vault"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PinCopilotModel, "from-env")
	if got, src := CopilotModel(reg); got != "from-vault" || src != "pin" {
		t.Errorf("CopilotModel(env set) = (%q, %q), want (from-vault, pin): env is not a pin source", got, src)
	}
}

// CodexModel resolves CODEX_MODEL: the pin if set, else "" (codex's own default decides, not the registry).
func TestCodexModelResolverPinVsDefault(t *testing.T) {
	load(t)
	t.Setenv(PinCodexModel, "")
	if err := SetPins(map[string]string{PinCodexModel: ""}); err != nil {
		t.Fatal(err)
	}
	if got, src := CodexModel(); got != "" || src != "default" {
		t.Errorf("CodexModel(unset) = (%q, %q), want (\"\", default)", got, src)
	}
	if err := SetPins(map[string]string{PinCodexModel: "gpt-6.1-sol"}); err != nil {
		t.Fatal(err)
	}
	if got, src := CodexModel(); got != "gpt-6.1-sol" || src != "pin" {
		t.Errorf("CodexModel(pinned) = (%q, %q), want (gpt-6.1-sol, pin)", got, src)
	}
}

// CodexEffort resolves CODEX_EFFORT the same way; a process env var is not a pin source.
func TestCodexEffortResolverPinVsDefault(t *testing.T) {
	load(t)
	t.Setenv(PinCodexEffort, "")
	if err := SetPins(map[string]string{PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}
	if got, src := CodexEffort(); got != "" || src != "default" {
		t.Errorf("CodexEffort(unset) = (%q, %q), want (\"\", default)", got, src)
	}
	if err := SetPins(map[string]string{PinCodexEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if got, src := CodexEffort(); got != "high" || src != "pin" {
		t.Errorf("CodexEffort(pinned) = (%q, %q), want (high, pin)", got, src)
	}
	t.Setenv(PinCodexEffort, "low")
	if got, src := CodexEffort(); got != "high" || src != "pin" {
		t.Errorf("CodexEffort(env set) = (%q, %q), want (high, pin): env is not a pin source", got, src)
	}
}

// GeminiModel resolves GEMINI_MODEL from the vault only: the pin if set, else "" (agy's own default decides).
func TestGeminiModelResolverPinVsDefaultIgnoresEnv(t *testing.T) {
	load(t)
	t.Setenv(PinGeminiModel, "env-never-read")
	if err := SetPins(map[string]string{PinGeminiModel: ""}); err != nil {
		t.Fatal(err)
	}
	if got, src := GeminiModel(); got != "" || src != "default" {
		t.Errorf("GeminiModel(unset) = (%q, %q), want (\"\", default): env must not leak in", got, src)
	}
	if err := SetPins(map[string]string{PinGeminiModel: "gemini-3.8-flash-high"}); err != nil {
		t.Fatal(err)
	}
	if got, src := GeminiModel(); got != "gemini-3.8-flash-high" || src != "pin" {
		t.Errorf("GeminiModel(pinned) = (%q, %q), want (gemini-3.8-flash-high, pin)", got, src)
	}
}

// TestSelectDoesNotTranslateLegacyDatabricksPin: a databricks-<env>[-oai] pin is taken as written, never mapped to <env>.
func TestSelectDoesNotTranslateLegacyDatabricksPin(t *testing.T) {
	reg := selectTestRegistry(t)
	row := *reg.Providers[reg.Default] // a migrated <env> preset row the old pin used to be rewritten to
	row.Name, row.Preset = "qa", PresetDatabricks
	reg.Providers["qa"] = &row
	for _, pin := range []string{"databricks-qa", "databricks-dev-oai"} {
		writeProviderPins(t, map[string]string{PinProfile: pin})
		sel, err := Select(reg, "")
		if err == nil || !strings.Contains(err.Error(), pin) {
			t.Errorf("Select with pin %q = %+v, %v; want an unknown-provider error naming the pin", pin, sel.Provider, err)
		}
	}
}
