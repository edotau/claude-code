package providers

import (
	"testing"

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
