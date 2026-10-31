package cli

import (
	"github.com/edotau/claude-code/internal/agent"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A validated env-shaped name may appear in the "not in the vault" error; PrintEnv avoids any prompt.
func TestAnthropicSecretMissingNamesKey(t *testing.T) {
	vaulttest.Use(t)
	const name = "NOPE_ABSENT_KEY_XYZ"
	t.Setenv(name, "")
	o := agent.AnthropicOptions{Mode: agent.AuthOAuth, Name: name, PrintEnv: true}
	_, err := anthropicSecret(o)
	if err == nil || !strings.Contains(err.Error(), name+" is not in the vault") {
		t.Fatalf("err = %v", err)
	}
}
