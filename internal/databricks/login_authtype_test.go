package databricks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/sdkport"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

func seedCLIProfileRow(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-dev", map[string]any{
		"kind": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"cliProfile": "dev",
		"auth": map[string]any{
			"type": "bearer", "command": "databricks auth token -p dev -o json | jq -r .access_token",
			"refreshCommand": "my-custom-refresh",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fakeLogin(t *testing.T, fn func(*providers.Provider) (time.Duration, error)) {
	t.Helper()
	old := loginU2M
	loginU2M = func(_ context.Context, p *providers.Provider, _ sdkport.LoginOptions) (time.Duration, error) {
		return fn(p)
	}
	t.Cleanup(func() { loginU2M = old })
}

func overlayDoc(t *testing.T) string {
	t.Helper()
	old, _, err := providers.UpdateOverlayDoc(false, func(map[string]json.RawMessage, *vault.Tx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return string(old)
}

func TestVaultLoginAuthTypeRejectsOther(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int {
		return login([]string{"--profile", "x", "--auth-type", "m2m"})
	})
	if code == 0 || !strings.Contains(stderr, "vault set") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestVaultLoginAuthTypeDryRun(t *testing.T) {
	seedCLIProfileRow(t)
	before := overlayDoc(t)
	called := false
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { called = true; return 0, nil })
	code, stdout, stderr := captureOutput(t, func() int {
		return login([]string{"--profile", "dbx-dev", "--auth-type", "u2m", "--dry-run"})
	})
	if code != 0 || called {
		t.Fatalf("exit %d called=%v stderr %q", code, called, stderr)
	}
	for _, want := range []string{`"authType": "u2m"`, "would log in to https://example.cloud.databricks.com", "kept custom auth.refreshCommand"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if after := overlayDoc(t); after != before {
		t.Errorf("dry-run changed the vault overlay")
	}
}

func TestVaultLoginAuthTypeWrites(t *testing.T) {
	seedCLIProfileRow(t)
	sawU2M := false
	fakeLogin(t, func(p *providers.Provider) (time.Duration, error) {
		sawU2M = p.Auth.AuthType == providers.AuthU2M
		return 30 * time.Minute, nil
	})
	code, stdout, stderr := captureOutput(t, func() int {
		return login([]string{"--profile", "dbx-dev", "--auth-type", "u2m"})
	})
	if code != 0 || !sawU2M || !strings.Contains(stdout, "logged in · token expires in 30m") {
		t.Fatalf("exit %d u2m=%v stdout %q stderr %q", code, sawU2M, stdout, stderr)
	}
	doc := overlayDoc(t)
	if !strings.Contains(doc, `"authType":"u2m"`) && !strings.Contains(doc, `"authType": "u2m"`) {
		t.Errorf("authType not written: %s", doc)
	}
	if strings.Contains(doc, "databricks auth token -p dev -o json") || !strings.Contains(doc, "my-custom-refresh") {
		t.Errorf("derived command not dropped or custom not kept: %s", doc)
	}
}

func TestVaultLoginAuthTypeFailurePrintsRollback(t *testing.T) {
	seedCLIProfileRow(t)
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { return 0, context.DeadlineExceeded })
	code, _, stderr := captureOutput(t, func() int {
		return login([]string{"--profile", "dbx-dev", "--auth-type", "u2m"})
	})
	if code == 0 || !strings.Contains(stderr, "providers.prev") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}
