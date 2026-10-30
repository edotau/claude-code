package agent

import (
	"strings"
	"testing"
)

// TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists checks that when opencode has a login for
// another provider (openai) but none for its own "opencode" plan, an opencode-go cell (prefix "opencode-go"
// maps to credential id "opencode") falls through to the OPENCODE_API_KEY vault key, erroring when that is
// absent too — precedence never borrows the unrelated openai login.
func TestOpencodeCredentialFallsThroughWhenOnlyAnotherLoginExists(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	t.Setenv(OpencodeAPIKeyEnv, "")
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	if l := OpencodeLogins(); len(l) != 1 || l[0] != "openai" {
		t.Fatalf("precondition: logins = %v, want only openai", l)
	}
	env, err := opencodeCredential("opencode-go")
	if err == nil {
		t.Fatalf("env = %v, want an error: the openai login must not cover an opencode-go cell", env)
	}
	if !strings.Contains(err.Error(), OpencodeAPIKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, OpencodeAPIKeyEnv)
	}
}

// TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin checks the same opencode-go cell resolves once the
// OPENCODE_API_KEY vault key is present, still with no "opencode" login stored.
func TestOpencodeCredentialVaultKeyOutranksMissingOwnLogin(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	t.Setenv(OpencodeAPIKeyEnv, "sk-test-key")
	env, err := opencodeCredential("opencode-go")
	if err != nil {
		t.Fatalf("err = %v, want the vault/env key to resolve it", err)
	}
	if len(env) != 1 || env[0] != OpencodeAPIKeyEnv+"=sk-test-key" {
		t.Errorf("env = %v, want [%s=sk-test-key]", env, OpencodeAPIKeyEnv)
	}
}
