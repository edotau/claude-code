package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// `secrets set COPILOT_GITHUB_TOKEN` refuses a value that doesn't have the shape of a real GitHub
// token and stores nothing.
func TestSecretsSetCopilotTokenRefusesShortValue(t *testing.T) {
	vaulttest.Use(t)
	withStdin(t, "ok\n")

	code, _, stderr := captureOutput(t, func() int { return cmdSecretsSet([]string{"COPILOT_GITHUB_TOKEN"}) })
	if code == 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN=ok exit = 0, want nonzero")
	}
	if !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("stderr = %q, want the not-a-GitHub-token refusal", stderr)
	}
	if _, _, ok := secrets.Get("COPILOT_GITHUB_TOKEN"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

// A valid-shaped GitHub token is accepted and sealed in the vault.
func TestSecretsSetCopilotTokenAcceptsValidShape(t *testing.T) {
	vaulttest.Use(t)
	const tok = "gh" + "p_validshapedFAKEtoken1234567890"
	withStdin(t, tok+"\n")

	code, _, stderr := captureOutput(t, func() int { return cmdSecretsSet([]string{"COPILOT_GITHUB_TOKEN"}) })
	if code != 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN exit = %d, stderr=%q", code, stderr)
	}
	if v, _, ok := secrets.Get("COPILOT_GITHUB_TOKEN"); !ok || v != tok {
		t.Fatalf("COPILOT_GITHUB_TOKEN = (%q, %v), want %s stored", v, ok, tok)
	}
}
