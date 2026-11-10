package vault_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

func TestGitHubAuthWarnsWhenVaultUnreadable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := vault.Update(func(tx *vault.Tx) error { return tx.SetSchema(99) }); err != nil {
		t.Fatal(err)
	}
	var got string
	out := captureStderr(t, func() { got = vault.GitHubAuth() })
	if got != "" {
		t.Fatalf("GitHubAuth = %q, want empty", got)
	}
	if !strings.Contains(out, "cannot read "+vault.GitHubAuthKey) {
		t.Fatalf("stderr = %q, want a warning naming %s", out, vault.GitHubAuthKey)
	}
}
