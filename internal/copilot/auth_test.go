package copilot

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An unreadable registry hides any GitHub pin, so the launch must be refused, never run on another account's token.
func TestCopilotLaunchTokenRefusesOnRegistryLoadError(t *testing.T) {
	pinsScratch(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "other-account-token")
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")

	tok, err := CopilotLaunchToken()
	if err == nil || tok != "" {
		t.Fatalf("CopilotLaunchToken() = (%q, %v), want an error and no token", tok, err)
	}
	if got := CopilotToken(); got != "" {
		t.Errorf("CopilotToken() = %q, want no token on a registry load error", got)
	}
}
