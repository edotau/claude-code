package providers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
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
	if err := os.MkdirAll(filepath.Dir(paths.PinFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(paths.PinFile())
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range pins {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
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

func TestSelectReadsFilePinsWithSlotPrecedence(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProvider:           "gateway",
		PinModel:              "all-from-file",
		SlotPin(models.Opus):  "openai:gpt-5-nano",
		SlotPin(models.Haiku): "haiku-from-file",
	})

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gateway" {
		t.Fatalf("provider = %q, want gateway from pin file", sel.Provider.Name)
	}
	for _, slot := range models.Slots {
		want := Target{Provider: sel.Provider, Model: "all-from-file"}
		switch slot {
		case models.Opus:
			want = Target{Provider: reg.Providers["openai"], Model: "gpt-5-nano"}
		case models.Haiku:
			want.Model = "haiku-from-file"
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
		PinModel:             "all-from-file",
		SlotPin(models.Opus): "opus-from-file",
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
	if got := sel.Slots[models.Opus]; got.Provider != sel.Provider || got.Model != "opus-from-file" {
		t.Errorf("opus = %+v, want file value after whitespace-only env fallback", got)
	}
	if got := sel.Slots[models.Sonnet]; got.Provider != sel.Provider || got.Model != "all-from-env" {
		t.Errorf("sonnet = %+v, want trimmed env all-model pin", got)
	}

	t.Setenv(PinProvider, "")
	writeProviderPins(t, map[string]string{
		PinProvider:          "gw",
		SlotPin(models.Opus): "opus-from-file",
	})
	flagSel, err := Select(reg, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if flagSel.Provider.Name != "gateway" {
		t.Errorf("provider flag selected %q, want gateway over file pin gw", flagSel.Provider.Name)
	}
	if got := flagSel.Slots[models.Opus]; got.Provider != flagSel.Provider || got.Model != "opus-from-file" {
		t.Errorf("flag-selected opus = %+v, want file value on gateway", got)
	}
}

func TestSelectAllModelPinBeatsProviderDefaults(t *testing.T) {
	reg := selectTestRegistry(t)
	writeProviderPins(t, map[string]string{
		PinProvider: "gateway",
		PinModel:    "all-from-file",
	})

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range models.Slots {
		if got := sel.Slots[slot]; got.Provider != sel.Provider || got.Model != "all-from-file" {
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
