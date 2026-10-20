package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestScanVaultFindsInlineSecrets(t *testing.T) {
	vaulttest.Use(t)
	const value = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	vaulttest.Put(t, vault.Pins, "PIN_TOKEN", value)
	vaulttest.Put(t, vault.Env, "ENV_TOKEN", value)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"x":{"headers":[{"Authorization":"`+value+`"}]}}}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"o":{"launch":{"env":["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1","O_KEY=`+value+`"]}}}}`)
	vaulttest.Put(t, vault.Env, "STORED_REF", providers.SecretRef)
	vaulttest.Put(t, vault.Pins, "ENV_REF", "${API_KEY}")
	vaulttest.Secret(t, "SEALED", value)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	scanner.ScanVault()
	want := []string{
		"vault:overlay/providers → providers.x.headers.0.Authorization holds an inline secret",
		"vault:overlay/vendors → vendors.o.launch.env.1 holds an inline secret",
		"vault:pins → PIN_TOKEN holds an inline secret",
		"vault:env → ENV_TOKEN holds an inline secret",
	}
	if !slices.Equal(scanner.Findings, want) {
		t.Fatalf("ScanVault findings = %v, want %v", scanner.Findings, want)
	}
	for _, finding := range want {
		if !strings.Contains(out.String(), finding) {
			t.Errorf("ScanVault output missing %q: %s", finding, out.String())
		}
	}
	if strings.Contains(out.String(), value) {
		t.Fatal("ScanVault printed a secret value")
	}
}

func TestScanVaultClean(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "populated"}[populated], func(t *testing.T) {
			vaulttest.Use(t)
			if populated {
				vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "openai")
				vaulttest.Put(t, vault.Env, "API_KEY", providers.SecretRef)
				vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"x":{"auth":{"env":"API_KEY"}}}}`)
				vaulttest.Secret(t, "API_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz")
			}
			t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
			var out bytes.Buffer
			scanner := &Scanner{Out: &out}
			scanner.ScanVault()
			if len(scanner.Findings) != 0 || !strings.Contains(out.String(), "vault — clean") {
				t.Fatalf("ScanVault = %v, output %s", scanner.Findings, out.String())
			}
			if !populated {
				if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
					t.Fatalf("ScanVault created a vault: %v", err)
				}
			}
		})
	}
}

func TestScanVaultReadErrorIsFinding(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "3")
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	scanner.ScanVault()
	if len(scanner.Findings) != 1 || !strings.Contains(scanner.Findings[0], "vault — could not read:") ||
		!strings.Contains(scanner.Findings[0], "schema 3") || strings.Contains(out.String(), "vault — clean") {
		t.Fatalf("ScanVault must report unreadable vault: %v, %s", scanner.Findings, out.String())
	}
}

func TestScanConfigsDefaultIncludesVaultAndSummary(t *testing.T) {
	dir := vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	vaulttest.Put(t, vault.Env, "INLINE_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz")
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	if code := scanner.ScanConfigs(nil); code != ExitFindings {
		t.Fatalf("default scan = %d, want findings: %s", code, out.String())
	}
	if len(scanner.Findings) != 1 || scanner.Findings[0] != "vault:env → INLINE_KEY holds an inline secret" {
		t.Fatalf("default findings = %v", scanner.Findings)
	}
	want := "secrets scan: 1 finding(s) — move inline values into the vault with `claude-code secrets set`."
	if !strings.Contains(out.String(), path+" — clean") || !strings.Contains(out.String(), want) {
		t.Fatalf("default scan missing file scan or summary: %s", out.String())
	}
	out.Reset()
	scanner = &Scanner{Out: &out}
	if code := scanner.ScanConfigs([]string{path}); code != ExitClean || len(scanner.Findings) != 0 {
		t.Fatalf("explicit file scan included vault: %d, %v, %s", code, scanner.Findings, out.String())
	}
}

// TestScanVaultCoversPrevProvidersAndVendorsOverlays: an inline secret in overlay/providers.prev or
// overlay/vendors must surface exactly like overlay/providers — ScanVault must not scan providers only.
func TestScanVaultCoversPrevProvidersAndVendorsOverlays(t *testing.T) {
	vaulttest.Use(t)
	const value = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"providers":{"x":{"auth":{"env":"`+value+`"}}}}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"y":{"token":"`+value+`"}}}`)
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	scanner.ScanVault()
	want := []string{
		"vault:overlay/providers.prev → providers.x.auth.env holds an inline secret",
		"vault:overlay/vendors → vendors.y.token holds an inline secret",
	}
	if !slices.Equal(scanner.Findings, want) {
		t.Fatalf("ScanVault findings = %v, want %v", scanner.Findings, want)
	}
	if strings.Contains(out.String(), value) {
		t.Fatal("ScanVault printed a secret value")
	}
}
