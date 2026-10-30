package statusline

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// labelRegistry is the built-in rows plus a databricks qa preset row, swapped in for the registry seam.
func labelRegistry(t *testing.T) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["qa"] = &providers.Provider{Name: "qa", Provider: "databricks", Preset: providers.PresetDatabricks}
	prev := registry
	t.Cleanup(func() { registry = prev })
	registry = func() *providers.Registry { return reg }
}

func TestProviderLabelFamilyTable(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	for name, want := range map[string]string{
		"max":         "anthropic·max",
		"anthropic":   "anthropic",
		"qa":          "databricks·qa",
		"chatgpt":     "openai·chatgpt",
		"personal":    "github·personal",
		"antigravity": "google·antigravity",
		"nvidia":      "nvidia",
		"openai":      "openai",
		"corp-prod":   "corp-prod", // unknown row: unchanged
		"mystery":     "mystery",   // unknown row, no env suffix
	} {
		if got := providerLabel(name); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestModelFamily(t *testing.T) {
	for id, want := range map[string]string{
		"databricks-claude-opus-5-5":     "databricks",
		"databricks-claude-opus-5-5[1m]": "databricks",
		"claude-opus-5":                  "anthropic",
		"claude-opus-5[1m]":              "anthropic",
		"qwen3:30b":                      "",
		"":                               "",
	} {
		if got := modelFamily(id); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", id, got, want)
		}
	}
}

func renderPayload(t *testing.T, model string) string {
	t.Helper()
	payload := `{"workspace":{"current_dir":"/nowhere/proj"},"model":{"id":"` + model + `","display_name":"Opus 5"}}`
	return plain(Render([]byte(payload), "minimal"))
}

// Regression: another session's `switch` rewrites the vault pin; this session's label follows its own env.
func TestLabelTracksSessionEnvOverVaultPin(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	vaulttest.Put(t, vault.Pins, providers.PinProfile, "antigravity")
	t.Setenv(providers.EnvSessionProfile, "qa")
	if got := renderPayload(t, "claude-opus-5"); !strings.Contains(got, "databricks·qa") || strings.Contains(got, "antigravity") {
		t.Errorf("env session label: %q", got)
	}
	vaulttest.Put(t, vault.Pins, providers.PinProfile, "openai") // the other session switches again
	if got := renderPayload(t, "claude-opus-5"); !strings.Contains(got, "databricks·qa") || strings.Contains(got, "openai") {
		t.Errorf("label moved with the pin: %q", got)
	}
}

func TestLabelProviderBeforeModel(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "qa")
	got := renderPayload(t, "claude-opus-5")
	p, m := strings.Index(got, "databricks·qa"), strings.Index(got, "Opus 5")
	if p < 0 || m < 0 || p > m {
		t.Errorf("provider must precede model: %q", got)
	}
}

// L10: only the profile named for its provider collapses to bare "anthropic"; other anthropic-family rows keep their profile.
func TestProviderLabelAnthropicFamilyRows(t *testing.T) {
	hermetic(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["anthropic-work"] = &providers.Provider{Name: "anthropic-work", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Protocol: providers.ProtocolAnthropic}
	reg.Providers["acme"] = &providers.Provider{Name: "acme", Provider: "anthropic", BaseURL: "https://gw.anthropic.com", Protocol: providers.ProtocolAnthropic}
	prev := registry
	t.Cleanup(func() { registry = prev })
	registry = func() *providers.Registry { return reg }
	for name, want := range map[string]string{
		"anthropic": "anthropic", "anthropic-work": "anthropic·anthropic-work", "acme": "anthropic·acme",
	} {
		if got := providerLabel(name); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", name, got, want)
		}
	}
}
