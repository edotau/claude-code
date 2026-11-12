package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

func TestClientSecretLockedVaultErrors(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	kr.Down = true
	_, err := ClientSecret()
	if err == nil || !strings.Contains(err.Error(), "claude-code vault") {
		t.Fatalf("err = %v, want one pointing at `claude-code vault`", err)
	}
}

func writeProxyFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func removeProxyFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
