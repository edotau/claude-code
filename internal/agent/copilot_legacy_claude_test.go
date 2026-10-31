package agent

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
		if _, ok := providers.AnthropicRoute(p, id); !ok {
			t.Errorf("AnthropicRoute(%q) not ok, want the anthropic route", id)
		}
		if w, _, ok, _ := copilotEndpointWire(p, id); !ok || w != providers.ProtocolAnthropic {
			t.Errorf("copilotEndpointWire(%q) = %q,%v, want anthropic wire", id, w, ok)
		}
	}
	if _, ok := providers.AnthropicRoute(p, "databricks-kimi-k3"); ok {
		t.Error("AnthropicRoute(kimi) ok, want false")
	}
}
