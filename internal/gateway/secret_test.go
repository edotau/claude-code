package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

func TestClientSecretUnreadableLegacyFileErrorsWithoutMinting(t *testing.T) {
	vaulttest.Use(t)
	if err := os.MkdirAll(secretFile(), 0o700); err != nil { // a directory: ReadFile fails with EISDIR, not ENOENT
		t.Fatal(err)
	}
	_, err := ClientSecret()
	if err == nil || !strings.Contains(err.Error(), "proxy: read") {
		t.Fatalf("err = %v, want a read error", err)
	}
	if s, found, _ := vault.ReadSecret(secretName); found || s != "" {
		t.Fatalf("a secret was minted into the vault: %q", s)
	}
}

func TestClientSecretRetriesLegacyDeletionOnVaultHit(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, secretName, "already-imported-secret")
	child := filepath.Join(secretFile(), "blocks-removal")
	writeProxyFixture(t, child, "interrupted migration")
	if _, err := ClientSecret(); err == nil || !strings.Contains(err.Error(), "remove legacy") {
		t.Fatalf("ClientSecret with blocked cleanup = %v, want legacy removal error", err)
	}
	removeProxyFixture(t, child)
	removeProxyFixture(t, secretFile())
	writeProxyFixture(t, secretFile(), "already-imported-secret\n")
	got, err := ClientSecret()
	if err != nil || got != "already-imported-secret" {
		t.Fatalf("ClientSecret retry = %q, %v; want the existing vault secret", got, err)
	}
	if _, err := os.Stat(secretFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy secret survived successful retry: %v", err)
	}
}
