package proxy

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAuthHintByAuthType(t *testing.T) {
	cases := map[string]struct {
		p    providers.Provider
		want string
	}{
		"oauth":   {providers.Provider{Name: "d", Auth: providers.Auth{Type: providers.AuthOAuth}}, "claude-code databricks login --profile d"},
		"preset":  {providers.Provider{Name: "d", Preset: providers.PresetDatabricks}, "claude-code databricks login --profile d"},
		"dbx pat": {providers.Provider{Name: "d", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthPAT}}, "--profile d --auth-type oauth"},
		"key":     {providers.Provider{Name: "k", Auth: providers.Auth{Type: providers.AuthKey, Env: "K_KEY"}}, "claude-code vault set K_KEY"},
		"command": {providers.Provider{Name: "c", Auth: providers.Auth{Type: providers.AuthKey, Command: "op read x"}}, "auth.command"},
		"other":   {providers.Provider{Name: "o", Auth: providers.Auth{Type: providers.AuthCLI}}, "claude-code configure provider --list o"},
	}
	for name, c := range cases {
		if got := authHint(&c.p); !strings.Contains(got, c.want) {
			t.Errorf("%s: authHint = %q, want %q", name, got, c.want)
		}
	}
}

func TestClientSecretImportsLegacyFileThenDeletesIt(t *testing.T) {
	vaulttest.Use(t)
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretFile(), []byte("legacy-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ClientSecret()
	if err != nil || got != "legacy-value" {
		t.Fatalf("ClientSecret = %q, %v; want the legacy value", got, err)
	}
	if _, err := os.Stat(secretFile()); !os.IsNotExist(err) {
		t.Fatalf("legacy file survives the import: %v", err)
	}
	if again, _ := ClientSecret(); again != "legacy-value" {
		t.Fatalf("second read = %q, want the vault copy", again)
	}
}

func TestClientSecretConcurrentFirstInitYieldsOneValue(t *testing.T) {
	vaulttest.Use(t)
	const n = 8
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { out[i], _ = ClientSecret() })
	}
	wg.Wait()
	for _, v := range out {
		if v == "" || v != out[0] {
			t.Fatalf("secrets differ or empty: %q", out)
		}
	}
}

func TestClientSecretLockedVaultErrorsWithoutWritingAFile(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	kr.Down = true
	_, err := ClientSecret()
	if err == nil || !strings.Contains(err.Error(), "claude-code vault") {
		t.Fatalf("err = %v, want one pointing at `claude-code vault`", err)
	}
	if _, serr := os.Stat(secretFile()); !os.IsNotExist(serr) {
		t.Fatalf("a secret file was written: %v", serr)
	}
}
