package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

const gapCfg = `[DEFAULT]
host = https://leak.example.cloud.databricks.com
token = dapi-DEFAULT-LEAK

[svc-m2m]
host = https://svc.example.cloud.databricks.com
client_id = cid-1
client_secret = secret-value-1

[pat-one]
host = https://pat.example.cloud.databricks.com
token = dapi-token-value
`

func gapImport(t *testing.T, extra ...string) (code int, out string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(gapCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"import", "--from", "databrickscfg", "--file", cfg}, extra...)
	code, stdout, stderr := captureOutput(t, func() int { return cmdVault(args) })
	return code, stdout + stderr
}

func gapProviderNames(t *testing.T) []string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range reg.Providers {
		names = append(names, n)
	}
	return names
}

func TestVaultImportDryRunWithSealFlagWritesNothing(t *testing.T) {
	useVaultTest(t)
	before := gapProviderNames(t)
	code, out := gapImport(t, "--seal-secrets")
	if code != 0 || !strings.Contains(out, "dry-run") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if after := gapProviderNames(t); len(after) != len(before) || slices.Contains(after, "svc-m2m") {
		t.Errorf("dry-run created rows: %v", after)
	}
	for _, n := range secrets.Names() {
		if strings.HasPrefix(n, "PROVIDER_") {
			t.Errorf("dry-run sealed %s", n)
		}
	}
	for _, leak := range []string{"secret-value-1", "dapi-token-value", "dapi-DEFAULT-LEAK", "leak.example"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q", leak)
		}
	}
}

func TestVaultImportApplyWithoutSealStoresNoSecrets(t *testing.T) {
	useVaultTest(t)
	if code, out := gapImport(t, "--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !slices.Contains(gapProviderNames(t), "svc-m2m") {
		t.Fatal("row not written")
	}
	for _, n := range secrets.Names() {
		if strings.HasPrefix(n, "PROVIDER_") {
			t.Errorf("sealed %s without --seal-secrets", n)
		}
	}
}

func TestVaultImportSealStoresReadableValuesAndNoDefaultLeak(t *testing.T) {
	useVaultTest(t)
	if code, out := gapImport(t, "--apply", "--seal-secrets"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for name, want := range map[string]string{
		"PROVIDER_SVC_M2M_CLIENT_SECRET": "secret-value-1",
		"PROVIDER_PAT_ONE_PAT":           "dapi-token-value",
	} {
		if got, src, err := secrets.Lookup(name); err != nil || got != want || src != "vault" {
			t.Errorf("%s = %q (%s) %v", name, got, src, err)
		}
	}
	for _, n := range secrets.Names() {
		if v, _, _ := secrets.Lookup(n); v == "dapi-DEFAULT-LEAK" {
			t.Errorf("[DEFAULT] token sealed as %s", n)
		}
	}
}

func TestVaultImportExistingRowSkippedUnlessReplace(t *testing.T) {
	useVaultTest(t)
	if err := providers.SetProviderFields("pat-one", map[string]any{
		"kind": "anthropic", "base_url": "https://old.example.cloud.databricks.com/serving-endpoints/anthropic",
		"auth": map[string]any{"type": "bearer", "authType": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	host := func() string {
		reg, err := providers.Load()
		if err != nil {
			t.Fatal(err)
		}
		return reg.Providers["pat-one"].BaseURL
	}
	if code, out := gapImport(t, "--apply"); code != 0 || !strings.Contains(out, "row exists") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(host(), "old.example") {
		t.Errorf("existing row overwritten without --replace: %s", host())
	}
	if code, out := gapImport(t, "--apply", "--replace"); code != 0 {
		t.Fatalf("replace exit %d: %s", code, out)
	}
	if strings.Contains(host(), "old.example") {
		t.Errorf("--replace did not overwrite: %s", host())
	}
}

// SealSecret (used by login/mint) and `vault set` (secrets.Store) must land the same visible record.
func TestSealSecretVisibleLikeVaultSet(t *testing.T) {
	useVaultTest(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := secrets.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("ZZ_SEALED_SECRET", val); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ZZ_STORE_SECRET", "ZZ_SEALED_SECRET"} {
		if got, src, err := secrets.Lookup(n); err != nil || got != val || src != "vault" {
			t.Errorf("Lookup(%s) = %q (%s) %v", n, got, src, err)
		}
	}
	code, out, _ := captureOutput(t, func() int { return cmdVault([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "ZZ_STORE_SECRET") || !strings.Contains(out, "ZZ_SEALED_SECRET") {
		t.Fatalf("vault list: %d %q", code, out)
	}
	var doc struct {
		Secrets []struct {
			Name, Masked string
			Length       int
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("vault list output not JSON: %v", err)
	}
	got := map[string]string{}
	for _, e := range doc.Secrets {
		got[e.Name] = fmt.Sprintf("%s/%d", e.Masked, e.Length)
	}
	if got["ZZ_STORE_SECRET"] == "" || got["ZZ_STORE_SECRET"] != got["ZZ_SEALED_SECRET"] {
		t.Errorf("vault list renders them differently: %v", got)
	}
}
