package agent

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func argvModel(argv []string) string {
	if i := slices.Index(argv, "--model"); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// Flags beat the agent's profile pin; a pin fills what the flag left empty; AGENT_ACTIVE=off ignores it.
func TestWithAgentPinsPrecedence(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "copilot", "profile", "max")
	vaulttest.SetRecord(t, "copilot", "model", "kimi-k3")
	if got := withAgentPins("copilot", LaunchOptions{}); got.Provider != "max" || got.Model != "" {
		t.Errorf("pins only = %+v, want max and no model (the model pin applies after the target resolves)", got)
	}
	if got := withAgentPins("copilot", LaunchOptions{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if got := withAgentPins("copilot", LaunchOptions{}); got.Provider != "" || got.Model != "" {
		t.Errorf("COPILOT_ACTIVE=off = %+v, want the pins ignored", got)
	}
}

const pinDbxRow = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"pat"}}}}}`

// A model pin follows the pinned profile into the target; the flag beats it; a Databricks id is dropped off Databricks.
func TestAgentModelTargetPin(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	const m = "databricks-gpt-6-1-sol"
	vaulttest.SetRecord(t, "codex", "profile", "dev")
	vaulttest.SetRecord(t, "codex", "model", m)
	if got := withAgentPins("codex", LaunchOptions{}); got.Provider != "dev" || got.Model != "" {
		t.Fatalf("codex = %+v, want the profile only", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := providers.Target{Provider: reg.Providers["dev"], Model: "base"}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: dev}); got.Model != m {
		t.Errorf("dev = %q, want %s kept", got.Model, m)
	}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Flag: "flag-m", Target: dev}); got.Model == m {
		t.Errorf("explicit --model lost to the pin: %q", got.Model)
	}
	off := providers.Target{Provider: &providers.Provider{Name: "chatgpt"}, Model: "base"}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: off}); got.Model != "base" {
		t.Errorf("chatgpt = %q, want the databricks id skipped", got.Model)
	}
}
