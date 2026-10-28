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
		"auth": map[string]any{"type":"oauth"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVaultLoginDryRun(t *testing.T) {
	seedU2MRow(t)
	code, stdout, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m", "--dry-run"}) })
	if code != 0 || !strings.Contains(stdout, "DATABRICKS_DBX_U2M_OAUTH") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestVaultLoginRejectsOtherKinds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return login([]string{"--profile", "openai", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "has auth type") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestVaultLoginRefusesRowWithAnotherAuthType(t *testing.T) {
	useVaultTest(t)
	err := providers.SetProviderFields("dbx-pat", map[string]any{
		"kind": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type":"pat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-pat", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "--auth-type oauth") {
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

// -p is --profile's shorthand on both commands.
func TestLoginAndMintTakeShorthandP(t *testing.T) {
	for _, run := range []func([]string) int{login, mint} {
		_, _, stderr := captureOutput(t, func() int { return run([]string{"-p", "no-such-row", "--dry-run"}) })
		if !strings.Contains(stderr, "no-such-row") || strings.Contains(stderr, "flag provided but not defined") {
			t.Fatalf("stderr %q, want -p to select the row no-such-row", stderr)
		}
	}
}
