package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestProviderListJSONCarriesHostNoSecret(t *testing.T) {
	useVaultTest(t)
	const secret = "dapi-list-secret-value-123456"
	err := providers.SetProviderFields("dbx-host", map[string]any{
		"kind": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "bearer", "authType": "pat", "accountId": "1234abcd-0000", "workspaceId": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("PROVIDER_DBX_HOST_PAT", secret); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int { return configureProviderList("dbx-host", "json") })
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var e struct{ Host, AccountID, WorkspaceID string }
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	e.Host, _ = raw["host"].(string)
	e.AccountID, _ = raw["accountId"].(string)
	e.WorkspaceID, _ = raw["workspaceId"].(string)
	if e.Host != "https://example.cloud.databricks.com" || e.AccountID != "1234abcd-0000" || e.WorkspaceID != "42" {
		t.Errorf("host/ids = %+v\n%s", e, out)
	}
	if strings.Contains(out, secret) {
		t.Error("list output carries the sealed secret")
	}
	code, all, _ := captureOutput(t, func() int { return configureProviderList("", "json") })
	if code != 0 || !strings.Contains(all, `"host"`) || strings.Contains(all, secret) {
		t.Errorf("full list: exit %d, secret leaked or host missing", code)
	}
}

func TestUnsupportedAuthTypeWriteRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-bad", map[string]any{
		"kind": "anthropic", "base_url": "https://example.cloud.databricks.com/serving-endpoints/anthropic",
		"auth": map[string]any{"type": "bearer", "authType": "external-browser"},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported (supported: databricks-cli, oauth-m2m, pat)") {
		t.Fatalf("write err = %v", err)
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers["dbx-bad"] != nil {
		t.Errorf("rejected row landed: err=%v", err)
	}
}
