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
	code, stdout, stderr := captureOutput(t, func() int { return login([]string{"dbx-u2m", "--dry-run"}) })
	if code != 0 || !strings.Contains(stdout, "PROVIDER_DBX_U2M_REFRESH_TOKEN") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestVaultLoginRejectsOtherKinds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return login([]string{"openai", "--dry-run"}) })
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
	code, _, stderr := captureOutput(t, func() int { return login([]string{"dbx-pat", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "--auth-type u2m") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}
