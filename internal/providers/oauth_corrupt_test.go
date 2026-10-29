package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestReadOAuthCorruptSecretIsAnError(t *testing.T) {
	p := mintRow(t, "corrupt-oauth", AuthOAuth, "", "http://127.0.0.1:1")
	if err := SealSecret(AuthSecretName(p), "not-json{"); err != nil {
		t.Fatal(err)
	}
	if _, err := readOAuth(p); err == nil {
		t.Fatal("readOAuth returned nil error for an unparseable existing secret")
	}
}

func TestCopilotModelNilRegistry(t *testing.T) {
	vaulttest.UseKeyring(t)
	if m, _ := CopilotModel(nil); m != "" {
		t.Fatalf("model = %q, want empty", m)
	}
}

func TestSealAccessKeepsUnreadableOAuthValue(t *testing.T) {
	p := mintRow(t, "seal-unreadable", AuthOAuth, "", "http://127.0.0.1:1")
	if err := SealSecret(AuthSecretName(p), "not-json{"); err != nil {
		t.Fatal(err)
	}
	sealAccess(p, "new-access")
	raw, _, err := vault.ReadSecret(AuthSecretName(p))
	if err != nil || raw != "not-json{" {
		t.Fatalf("sealed value = %q, %v; want it untouched after an unreadable read", raw, err)
	}
}
