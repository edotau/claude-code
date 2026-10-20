package providers

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSecretsFile plants name=value in the file plane for EffectiveSecret's lowest rung.
func writeSecretsFile(t *testing.T, name, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(SecretsFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SecretsFile(), []byte(name+"="+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// EffectiveSecret must try env, then keyring, then the file plane, in that order, and miss cleanly.
func TestEffectiveSecretOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeSecretsFile(t, "ORDER_NAME", "from-file")
	stubKeyring(t, map[string]string{"ORDER_NAME": "from-keyring"})

	if v, src := EffectiveSecret("ORDER_NAME"); v != "from-keyring" || src != "keyring" {
		t.Errorf("EffectiveSecret (keyring beats file) = (%q, %q), want (from-keyring, keyring)", v, src)
	}

	t.Setenv("ORDER_NAME", "from-env")
	if v, src := EffectiveSecret("ORDER_NAME"); v != "from-env" || src != "env" {
		t.Errorf("EffectiveSecret (env beats keyring) = (%q, %q), want (from-env, env)", v, src)
	}
}

// A whitespace-only env var must be treated as unset, not as a winning value.
func TestEffectiveSecretIgnoresWhitespaceOnlyEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubKeyring(t, map[string]string{"WS_NAME": "from-keyring"})
	t.Setenv("WS_NAME", "   ")

	if v, src := EffectiveSecret("WS_NAME"); v != "from-keyring" || src != "keyring" {
		t.Errorf("EffectiveSecret with whitespace-only env = (%q, %q), want (from-keyring, keyring)", v, src)
	}
}

// A name held nowhere must miss cleanly: ("", "").
func TestEffectiveSecretMiss(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubKeyring(t, nil)

	if v, src := EffectiveSecret("NEVER_SET_ANYWHERE"); v != "" || src != "" {
		t.Errorf("EffectiveSecret(miss) = (%q, %q), want (\"\", \"\")", v, src)
	}
}
