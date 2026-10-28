package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestClaudeLoginStatusLoggedIn(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
echo '{"loggedIn":true,"subscriptionType":"max"}'`)
	loggedIn, plan := ClaudeLoginStatus()
	if !loggedIn || plan != " (max)" {
		t.Errorf("ClaudeLoginStatus() = %v, %q; want true, \" (max)\"", loggedIn, plan)
	}
}

func TestClaudeLoginStatusNoBinary(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	loggedIn, plan := ClaudeLoginStatus()
	if loggedIn || plan != "" {
		t.Errorf("ClaudeLoginStatus() with no claude binary = %v, %q; want false, \"\"", loggedIn, plan)
	}
}

// TestHeadlessOAuthToken covers the two sources in priority order: env, then the vault.
func TestHeadlessOAuthToken(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(OAuthTokenEnv, "")
	if got := HeadlessOAuthToken(); got != "" {
		t.Errorf("HeadlessOAuthToken() with nothing set = %q, want \"\"", got)
	}

	vaulttest.Secret(t, OAuthTokenEnv, "from-vault")
	if got := HeadlessOAuthToken(); got != "from-vault" {
		t.Errorf("HeadlessOAuthToken() (vault) = %q, want from-vault", got)
	}

	t.Setenv(OAuthTokenEnv, "from-env")
	if got := HeadlessOAuthToken(); got != "from-env" {
		t.Errorf("HeadlessOAuthToken() (env beats vault) = %q, want from-env", got)
	}
}

// lockVault seeds name in the vault, then swaps HARNESS_VAULT_KEY for a different key with an empty
// keyring behind it, so the record exists but nothing can open it.
func lockVault(t *testing.T, name, value string) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Keyring(t)
	vaulttest.Secret(t, name, value)
	if v, _, err := EffectiveSecret(name); err != nil || v != value {
		t.Fatalf("precondition: EffectiveSecret(%s) = %q, %v; want %q", name, v, err, value)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if _, _, err := EffectiveSecret(name); err == nil {
		t.Fatalf("precondition: EffectiveSecret(%s) under the wrong key must error", name)
	}
}

// R14: a vault that holds the name but cannot open it must not block auth.file or auth.command.
func TestCredentialLockedVaultFallsThrough(t *testing.T) {
	lockVault(t, "LOCKED_KEY", "sealed-value")
	t.Setenv("LOCKED_KEY", "")
	ctx := context.Background()

	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Name: "lk", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY", File: file}}
	if v, err := Credential(ctx, p); err != nil || v != "from-file" {
		t.Errorf("Credential (locked vault, auth.file) = %q, %v; want from-file", v, err)
	}

	p = &Provider{Name: "lk-cmd", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY", Command: "echo minted-locked"}}
	if v, err := Credential(ctx, p); err != nil || v != "minted-locked" {
		t.Errorf("Credential (locked vault, auth.command) = %q, %v; want minted-locked", v, err)
	}

	p = &Provider{Name: "lk-none", Auth: Auth{Type: AuthKey, Env: "LOCKED_KEY"}}
	v, err := Credential(ctx, p)
	if err == nil || v != "" || !strings.Contains(err.Error(), "lk-none: no credential (vault:") {
		t.Errorf("Credential (locked vault, nothing else) = %q, %v; want a \"no credential (vault: …)\" error", v, err)
	}

	t.Setenv("LOCKED_KEY", "from-env")
	if v, err := Credential(ctx, p); err != nil || v != "from-env" {
		t.Errorf("Credential (env over a locked vault) = %q, %v; want from-env", v, err)
	}
}

// A provider with nothing configured anywhere gets the set-it hint, not a vault error.
func TestCredentialMissHint(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("MISS_KEY", "")
	p := &Provider{Name: "miss", Auth: Auth{Type: AuthKey, Env: "MISS_KEY", File: "/nonexistent/tok"}}
	_, err := Credential(context.Background(), p)
	if err == nil || strings.Contains(err.Error(), "vault:") ||
		!strings.Contains(err.Error(), "set MISS_KEY with `claude-code vault set`, or /nonexistent/tok") {
		t.Errorf("Credential(miss) err = %v; want the secrets-set hint naming MISS_KEY and auth.file", err)
	}
}

// CredentialPresent never decrypts: a sealed record, auth.file and auth.command all count under a wrong key.
func TestCredentialPresentWithoutDecrypting(t *testing.T) {
	lockVault(t, "PRESENT_KEY", "sealed-value")
	t.Setenv("PRESENT_KEY", "")
	t.Setenv("ABSENT_KEY", "")

	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "PRESENT_KEY"}}) {
		t.Error("CredentialPresent must report a sealed vault record without opening it")
	}
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", File: file}}) {
		t.Error("CredentialPresent must keep auth.file as a source")
	}
	blank := filepath.Join(t.TempDir(), "blank")
	if err := os.WriteFile(blank, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", File: blank}}) {
		t.Error("CredentialPresent must not count a blank auth.file")
	}
	if !CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY", Command: "exit 1"}}) {
		t.Error("CredentialPresent must keep auth.command as a source (without running it)")
	}
	if CredentialPresent(&Provider{Auth: Auth{Type: AuthKey, Env: "ABSENT_KEY"}}) {
		t.Error("CredentialPresent with nothing configured must be false")
	}
	for _, typ := range []string{AuthNone, AuthCLI} {
		if !CredentialPresent(&Provider{Auth: Auth{Type: typ}}) {
			t.Errorf("CredentialPresent(%s) must be true", typ)
		}
	}
}
