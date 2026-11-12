package vault

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGitHubAuthWarnsWhenVaultUnreadable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "schema", "99") }); err != nil {
		t.Fatal(err)
	}
	var got string
	out := captureStderr(t, func() { got = GitHubAuth() })
	if got != "" {
		t.Fatalf("GitHubAuth = %q, want empty", got)
	}
	if !strings.Contains(out, "cannot read "+gitHubAuthKey) {
		t.Fatalf("stderr = %q, want a warning naming %s", out, gitHubAuthKey)
	}
}
