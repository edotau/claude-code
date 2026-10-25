package cli

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func useVaultTest(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

func TestVaultSetCopilotTokenRefusesShortValue(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "ok\n")

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code == 0 {
		t.Fatal("set COPILOT_GITHUB_TOKEN accepted a short value")
	}
	if !strings.Contains(stderr, "not a GitHub token") {
		t.Fatalf("stderr = %q, want the not-a-GitHub-token refusal", stderr)
	}
	if _, _, ok := secrets.Get("COPILOT_GITHUB_TOKEN"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

func TestVaultSetTailscaleCredentialRefusesMalformedShape(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "not-a-client-id\n")

	code, _, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "TAILSCALE_OAUTH_CLIENT_ID"}) })
	if code == 0 {
		t.Fatal("set TAILSCALE_OAUTH_CLIENT_ID accepted a malformed value")
	}
	if !strings.Contains(stderr, "want a client ID") {
		t.Fatalf("stderr = %q, want the client-ID shape refusal", stderr)
	}
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		t.Fatal("a refused value must not be stored")
	}
}

// A valid-shaped GitHub token is accepted and sealed in the vault.
func TestVaultSetCopilotTokenAcceptsValidShape(t *testing.T) {
	useVaultTest(t)
	const tok = "gh" + "p_validshapedFAKEtoken1234567890"
	withStdin(t, tok+"\n")

	code, stdout, stderr := captureOutput(t, func() int { return cmdVault([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code != 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN exit = %d", code)
	}
	if v, _, ok := secrets.Get("COPILOT_GITHUB_TOKEN"); !ok || secrets.Redact(v) != secrets.Redact(tok) {
		t.Fatal("COPILOT_GITHUB_TOKEN was not stored correctly")
	}
	if strings.Contains(stdout+stderr, tok) {
		t.Fatal("set leaked the raw value")
	}
	if !strings.Contains(stderr, secrets.Redact(tok)) {
		t.Fatal("set did not report the masked value")
	}
}

func TestVaultSetNameDryRunStoresNothing(t *testing.T) {
	useVaultTest(t)
	const value = "obviously-fake-dry-run"
	withStdin(t, value+"\n")
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"set", "VAULT_TEST_DUMMY", "--dry-run"})
	})
	if code != 0 || !strings.Contains(stdout, "would add VAULT_TEST_DUMMY") {
		t.Fatalf("set --dry-run: exit %d, expected preview=%v",
			code, strings.Contains(stdout, "would add VAULT_TEST_DUMMY"))
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("dry run leaked the raw value")
	}
	if len(secrets.Names()) != 0 {
		t.Fatal("dry run stored a secret")
	}
}

func TestVaultSetNameAndTailscaleRefused(t *testing.T) {
	useVaultTest(t)
	withStdin(t, "obviously-fake-unused\n")
	code, _, _ := captureOutput(t, func() int {
		return cmdVault([]string{"set", "VAULT_TEST_DUMMY", "--tailscale"})
	})
	if code != 2 || len(secrets.Names()) != 0 {
		t.Fatalf("set NAME --tailscale: exit %d, stored names=%v", code, secrets.Names())
	}
}

func TestVaultDeleteStoredSecret(t *testing.T) {
	useVaultTest(t)
	const value = "obviously-fake-delete"
	if err := secrets.Store("VAULT_TEST_DUMMY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdVault([]string{"delete", "VAULT_TEST_DUMMY"})
	})
	if code != 0 || len(secrets.Names()) != 0 {
		t.Fatalf("delete: exit %d, stored names=%v", code, secrets.Names())
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("delete leaked the raw value")
	}
}
