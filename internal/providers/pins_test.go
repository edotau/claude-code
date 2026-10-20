package providers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
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

// writeSecretsFile plants name=value in the file plane for EffectiveSecret's lowest rung.
func writeSecretsFile(t *testing.T, name, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(paths.SecretsFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.SecretsFile(), []byte(name+"="+value+"\n"), 0o600); err != nil {
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

// EffectiveSecret must try env, then keyring, then the file plane, in that order, and miss cleanly.
func TestEffectiveSecretOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeSecretsFile(t, "ORDER_NAME", "from-file")
	stubKeyring(t, map[string]string{"ORDER_NAME": "from-keyring"})

	if v, src := EffectiveSecret("ORDER_NAME"); v != "from-keyring" || src != "keyring" {
		t.Errorf("EffectiveSecret (keyring beats file) = (%q, %q), want (from-keyring, keyring)", v, src)
	}

	t.Setenv("ORDER_NAME", "from-env")
	if v, src := EffectiveSecret("ORDER_NAME"); v != "from-env" || src != "env" {
		t.Errorf("EffectiveSecret (env beats keyring) = (%q, %q), want (from-env, env)", v, src)
	}
}

// A whitespace-only env var must be treated as unset, not as a winning value.
func TestEffectiveSecretIgnoresWhitespaceOnlyEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubKeyring(t, map[string]string{"WS_NAME": "from-keyring"})
	t.Setenv("WS_NAME", "   ")

	if v, src := EffectiveSecret("WS_NAME"); v != "from-keyring" || src != "keyring" {
		t.Errorf("EffectiveSecret with whitespace-only env = (%q, %q), want (from-keyring, keyring)", v, src)
	}
}

// A name held nowhere must miss cleanly: ("", "").
func TestEffectiveSecretMiss(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubKeyring(t, nil)

	if v, src := EffectiveSecret("NEVER_SET_ANYWHERE"); v != "" || src != "" {
		t.Errorf("EffectiveSecret(miss) = (%q, %q), want (\"\", \"\")", v, src)
	}
}
