package databricks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

func seedU2MRow(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-u2m", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "oauth"},
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
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"},
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // scratch vault: never the real overlay
	for _, run := range []func([]string) int{login, mint} {
		_, _, stderr := captureOutput(t, func() int { return run([]string{"-p", "no-such-row", "--dry-run"}) })
		if !strings.Contains(stderr, "no-such-row") || strings.Contains(stderr, "flag provided but not defined") {
			t.Fatalf("stderr %q, want -p to select the row no-such-row", stderr)
		}
	}
}

func seedCLIProfileRow(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-dev", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"cliProfile": "dev",
		"auth": map[string]any{
			"type": "key", "command": "databricks auth token -p dev -o json | jq -r .access_token",
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
	loginU2M = func(_ context.Context, p *providers.Provider, _ oauth.LoginOptions) (time.Duration, error) {
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
		return login([]string{"--profile", "dbx-dev", "--auth-type", "oauth", "--dry-run"})
	})
	if code != 0 || called {
		t.Fatalf("exit %d called=%v stderr %q", code, called, stderr)
	}
	for _, want := range []string{`"type": "oauth"`, "would log in to https://example.cloud.databricks.com", "kept custom auth.refreshCommand"} {
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
		sawU2M = p.Auth.Type == providers.AuthOAuth
		return 30 * time.Minute, nil
	})
	code, stdout, stderr := captureOutput(t, func() int {
		return login([]string{"--profile", "dbx-dev", "--auth-type", "oauth"})
	})
	if code != 0 || !sawU2M || !strings.Contains(stdout, "logged in · token expires in 30m") {
		t.Fatalf("exit %d u2m=%v stdout %q stderr %q", code, sawU2M, stdout, stderr)
	}
	doc := overlayDoc(t)
	if !strings.Contains(doc, `"type":"oauth"`) && !strings.Contains(doc, `"type": "oauth"`) {
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
		return login([]string{"--profile", "dbx-dev", "--auth-type", "oauth"})
	})
	if code == 0 || !strings.Contains(stderr, "providers.prev") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

// -t is --auth-type's shorthand, and it counts as an explicit switch like the long form.
func TestLoginShorthandT(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return login([]string{"-p", "x", "-t", "m2m"}) })
	if !strings.Contains(stderr, `--auth-type "m2m"`) {
		t.Fatalf("stderr %q, want -t m2m refused like --auth-type m2m", stderr)
	}
}

func seedOAuthRow(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := providers.SetProviderFields("dbx-oauth", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "oauth"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func stubValid(t *testing.T, ok bool) {
	t.Helper()
	old := validU2M
	validU2M = func(context.Context, *providers.Provider) (time.Duration, bool) { return 40 * time.Minute, ok }
	t.Cleanup(func() { validU2M = old })
}

func TestLoginReusesValidCachedToken(t *testing.T) {
	seedOAuthRow(t)
	stubValid(t, true)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return time.Minute, nil })
	code, stdout, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-oauth"}) })
	if code != 0 || calls != 0 || !strings.Contains(stdout, "already logged in · expires in 40m") {
		t.Fatalf("exit %d calls %d stdout %q stderr %q", code, calls, stdout, stderr)
	}
}

func TestLoginExpiredTokenOpensLogin(t *testing.T) {
	seedOAuthRow(t)
	stubValid(t, false)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := captureOutput(t, func() int { return login([]string{"--profile", "dbx-oauth"}) })
	if code != 0 || calls != 1 || !strings.Contains(stdout, "logged in · token expires in 30m") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

func TestLoginForceSkipsReuse(t *testing.T) {
	seedOAuthRow(t)
	stubValid(t, true)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := captureOutput(t, func() int { return login([]string{"--profile", "dbx-oauth", "--force"}) })
	if code != 0 || calls != 1 || strings.Contains(stdout, "already") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

// With no Databricks profile, login and mint name the import command instead of `unknown provider ""`.
func TestNoDatabricksProfileNamesImport(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for name, run := range map[string]func([]string) int{"login": login, "mint": mint} {
		code, _, stderr := captureOutput(t, func() int { return run([]string{"--dry-run"}) })
		if code != 1 || !strings.Contains(stderr, "databricks import --apply") || strings.Contains(stderr, `unknown provider ""`) {
			t.Errorf("%s: exit %d, stderr %q, want the no-profile message", name, code, stderr)
		}
	}
}

// A defaulted m2m/pat row's refusal says it already signs in without a browser and names the oauth-profile login.
func TestLoginRefusalNamesNonBrowserSignIn(t *testing.T) {
	useVaultTest(t)
	err := providers.SetProviderFields("dbx-pat2", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-pat2", "--dry-run"}) })
	if !strings.Contains(stderr, "without a browser") || !strings.Contains(stderr, "login -p <oauth profile>") {
		t.Errorf("stderr %q, want the no-browser note and the oauth-profile hint", stderr)
	}
}

// Every bad flag value from checkLoginFlags is a usage error (2), --as ones included.
func TestCheckLoginFlagsAsIsUsageError(t *testing.T) {
	for _, as := range []string{"sp", "bogus"} {
		var code int
		var bad bool
		captureOutput(t, func() int { code, bad = checkLoginFlags(as, ""); return 0 })
		if !bad || code != 2 {
			t.Errorf("--as %s = (%d, %v), want (2, true)", as, code, bad)
		}
	}
}
