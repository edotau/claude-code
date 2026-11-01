package agent

import (
	"github.com/edotau/claude-code/internal/vault"
	"testing"
)

// A shell model env beats the vault model pin for a CLI agent.
func TestAgentModelPinShellEnvBeatsVault(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "vault-model"}); err != nil {
		t.Fatal(err)
	}
	if got := AgentModelPin("codex"); got != "vault-model" {
		t.Fatalf("vault pin = %q", got)
	}
	t.Setenv("CODEX_MODEL", "shell-model")
	if got := AgentModelPin("codex"); got != "shell-model" {
		t.Errorf("AgentModelPin = %q, want shell-model", got)
	}
}
