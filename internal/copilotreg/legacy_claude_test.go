package copilotreg

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression: legacy Claude ids the catalog does not list stay Claude (no OpenAI preference, anthropic wire).
func TestLegacyClaudeStaysAnthropic(t *testing.T) {
	vaulttest.Use(t)
	host := "https://h.example.com"
	p := &providers.Provider{
		Name: "dev", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-",
		Routes: map[string]string{providers.ProtocolAnthropic: host, providers.ProtocolOpenAI: host + "/serving-endpoints"},
	}
	for _, id := range []string{"claude-3-7-sonnet", "databricks-claude-3-7-sonnet", "us.anthropic.claude-3-5-sonnet-20241022-v2:0"} {
		if providers.PrefersOpenAI(p, id) {
			t.Errorf("PrefersOpenAI(%q) = true, want false", id)
		}
		if w, m := NonClaudeWire(p, providers.ProtocolAnthropic, id, true); w != "" || m != "" {
			t.Errorf("NonClaudeWire(%q) = %q,%q, want anthropic wire", id, w, m)
		}
	}
	if !providers.PrefersOpenAI(p, "databricks-kimi-k3") {
		t.Error("PrefersOpenAI(kimi) = false, want true")
	}
}
