package databricks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func seedU2MRow(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-u2m", map[string]any{
		"kind": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "bearer", "authType": "u2m"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVaultLoginDryRun(t *testing.T) {
	seedU2MRow(t)
	code, stdout, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m", "--dry-run"}) })
	if code != 0 || !strings.Contains(stdout, "PROVIDER_DBX_U2M_REFRESH_TOKEN") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestVaultLoginRejectsOtherKinds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return login([]string{"--profile", "openai", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "not an overlay row") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestVaultLoginRefusesRowWithAnotherAuthType(t *testing.T) {
	useVaultTest(t)
	err := providers.SetProviderFields("dbx-pat", map[string]any{
		"kind": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "bearer", "authType": "pat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-pat", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "--auth-type u2m") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// Commands take flags only: a bare row name is a usage error naming --profile.
func TestLoginAndMintRefusePositionalRow(t *testing.T) {
	for _, run := range []func([]string) int{login, mint} {
		code, _, stderr := captureOutput(t, func() int { return run([]string{"dev"}) })
		if code != 2 || !strings.Contains(stderr, "pass --profile dev") {
			t.Fatalf("exit %d stderr %q, want 2 and a --profile hint", code, stderr)
		}
	}
}

// No --profile: the vault default when it is a Databricks row, else HARNESS_PROVIDER's env, else qa.
func TestResolveProfileFallsBackToThePin(t *testing.T) {
	dbx := &providers.Provider{Name: "prod", Preset: providers.PresetDatabricks}
	reg := &providers.Registry{Default: "prod", Providers: map[string]*providers.Provider{"prod": dbx}}
	if got := resolveProfile(reg, "dev"); got != "dev" {
		t.Fatalf("explicit = %q, want dev", got)
	}
	if got := resolveProfile(reg, ""); got != "prod" {
		t.Fatalf("Databricks default = %q, want prod", got)
	}
	reg.Default = "subscription"
	reg.Providers["subscription"] = &providers.Provider{Name: "subscription"}
	t.Setenv("HARNESS_PROVIDER", "dev")
	if got := resolveProfile(reg, ""); got != "dev" {
		t.Fatalf("non-Databricks default = %q, want HARNESS_PROVIDER's dev", got)
	}
	t.Setenv("HARNESS_PROVIDER", "")
	if got := resolveProfile(reg, ""); got != defaultEnv {
		t.Fatalf("no pin = %q, want %q", got, defaultEnv)
	}
}
