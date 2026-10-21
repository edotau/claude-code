package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func selectTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := load(t)
	for _, key := range append([]string{PinProvider, PinModel}, slotPinKeys()...) {
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
		PinProvider:           "gateway",
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

func TestSelectEnvironmentAndProviderFlagPrecedence(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProvider:          "gateway",
		PinModel:             "all-from-vault",
		SlotPin(models.Opus): "opus-from-vault",
	})
	t.Setenv(PinProvider, "  gw  ")
	t.Setenv(PinModel, "  all-from-env  ")
	t.Setenv(SlotPin(models.Opus), "   ")

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gw" {
		t.Fatalf("provider = %q, want trimmed env pin gw", sel.Provider.Name)
	}
	if got := sel.Slots[models.Opus]; got.Provider != sel.Provider || got.Model != "opus-from-vault" {
		t.Errorf("opus = %+v, want vault value after whitespace-only env fallback", got)
	}
	if got := sel.Slots[models.Sonnet]; got.Provider != sel.Provider || got.Model != "all-from-env" {
		t.Errorf("sonnet = %+v, want trimmed env all-model pin", got)
	}

	t.Setenv(PinProvider, "")
	writeProviderPins(t, map[string]string{
		PinProvider:          "gw",
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
		PinProvider: "gateway",
		PinModel:    "all-from-vault",
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

	writeProviderPins(t, map[string]string{PinProvider: "gateway"})
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

// Copilot's two launch pins round-trip through SetPins/Pin like any other vault pin: unset reads "",
// a set value reads back, and clearing (value "") removes it again.
func TestCopilotEffortAndContextPinsRoundTrip(t *testing.T) {
	vaulttest.UseKeyring(t)
	for _, key := range []string{PinCopilotEffort, PinCopilotContext} {
		t.Setenv(key, "")
	}
	for _, key := range []string{PinCopilotEffort, PinCopilotContext} {
		if got := Pin(key); got != "" {
			t.Errorf("%s unset = %q, want \"\"", key, got)
		}
	}
	if err := SetPins(map[string]string{PinCopilotEffort: "xhigh", PinCopilotContext: "long_context"}); err != nil {
		t.Fatal(err)
	}
	if got := Pin(PinCopilotEffort); got != "xhigh" {
		t.Errorf("PinCopilotEffort = %q, want xhigh", got)
	}
	if got := Pin(PinCopilotContext); got != "long_context" {
		t.Errorf("PinCopilotContext = %q, want long_context", got)
	}
	if err := SetPins(map[string]string{PinCopilotEffort: "", PinCopilotContext: ""}); err != nil {
		t.Fatal(err)
	}
	if got := Pin(PinCopilotEffort); got != "" {
		t.Errorf("PinCopilotEffort cleared = %q, want \"\"", got)
	}
	if got := Pin(PinCopilotContext); got != "" {
		t.Errorf("PinCopilotContext cleared = %q, want \"\"", got)
	}
}

// A process env var for either key beats the vault pin, same precedence as every other pin.
func TestCopilotEffortAndContextEnvBeatsVaultPin(t *testing.T) {
	vaulttest.UseKeyring(t)
	if err := SetPins(map[string]string{PinCopilotEffort: "low", PinCopilotContext: "default"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PinCopilotEffort, "max")
	t.Setenv(PinCopilotContext, "long_context")
	if got := Pin(PinCopilotEffort); got != "max" {
		t.Errorf("Pin(PinCopilotEffort) = %q, want env value max", got)
	}
	if got := Pin(PinCopilotContext); got != "long_context" {
		t.Errorf("Pin(PinCopilotContext) = %q, want env value long_context", got)
	}
}
