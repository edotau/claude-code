package databricks

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// useVaultTest isolates the vault in a temp config dir with a fresh master key.
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

// captureOutput runs fn with stdout/stderr captured.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	return runCaptured(t, fn)
}
