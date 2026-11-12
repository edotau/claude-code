package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	if code == 0 || !strings.Contains(stderr, "--auth-type oauth") ||
		!strings.Contains(stderr, "without a browser") || !strings.Contains(stderr, "login -p <oauth profile>") {
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
	command, _ := providers.DatabricksCLICommands("dev")
	err := providers.SetProviderFields("dbx-dev", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"cliProfile": "dev",
		"auth": map[string]any{
			"type": "key", "command": command,
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

func stubValid(t *testing.T, ok bool) {
	t.Helper()
	old := validU2M
	validU2M = func(context.Context, *providers.Provider) (time.Duration, bool) { return 40 * time.Minute, ok }
	t.Cleanup(func() { validU2M = old })
}

func TestLoginReusesValidCachedToken(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, true)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return time.Minute, nil })
	code, stdout, stderr := captureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m"}) })
	if code != 0 || calls != 0 || !strings.Contains(stdout, "already logged in · expires in 40m") {
		t.Fatalf("exit %d calls %d stdout %q stderr %q", code, calls, stdout, stderr)
	}
}

func TestLoginExpiredTokenOpensLogin(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, false)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := captureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m"}) })
	if code != 0 || calls != 1 || !strings.Contains(stdout, "logged in · token expires in 30m") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

func TestLoginForceSkipsReuse(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, true)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := captureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m", "--force"}) })
	if code != 0 || calls != 1 || strings.Contains(stdout, "already") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

// With no Databricks profile, login and mint name the import command instead of `unknown profile ""`.
func TestNoDatabricksProfileNamesImport(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for name, run := range map[string]func([]string) int{"login": login, "mint": mint} {
		code, _, stderr := captureOutput(t, func() int { return run([]string{"--dry-run"}) })
		if code != 1 || !strings.Contains(stderr, "databricks import --apply") || strings.Contains(stderr, `unknown profile ""`) {
			t.Errorf("%s: exit %d, stderr %q, want the no-profile message", name, code, stderr)
		}
	}
}

// Every bad flag value from checkLoginFlags is a usage error (2), --as ones included.
func TestCheckLoginFlagsAsIsUsageError(t *testing.T) {
	for _, as := range []string{"auto", "bogus"} {
		var code int
		var bad bool
		captureOutput(t, func() int { code, bad = checkLoginFlags(as, ""); return 0 })
		if !bad || code != 2 {
			t.Errorf("--as %s = (%d, %v), want (2, true)", as, code, bad)
		}
	}
}

// spLoginRow seeds row dbx-sp (spClientId when paired) on a local token endpoint counting client-credentials mints.
func spLoginRow(t *testing.T, identity string, paired, secret bool) *atomic.Int32 {
	t.Helper()
	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() == nil && r.PostForm.Get("grant_type") == "client_credentials" {
			mints.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"sp-access-0001","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	sp := ""
	if paired {
		sp = `,"spClientId":"sp-client"`
	}
	seedOverlay(t, fmt.Sprintf(`{"providers":{"databricks":{"dbx-sp":{"protocol":"anthropic","base_url":%q,`+
		`"auth":{"type":"oauth"%s},"identity":%q}}}}`, srv.URL, sp, identity))
	if secret {
		if err := vault.Store("DATABRICKS_DBX_SP_M2M", "sp-secret-value"); err != nil {
			t.Fatal(err)
		}
	}
	return &mints
}

// login mints as the SP with no browser on identity sp, --as sp, or auto with a paired SP; else the browser login.
func TestLoginIdentitySPMintsWithoutBrowser(t *testing.T) {
	for _, c := range []struct {
		name, identity, as   string
		paired, secret       bool
		code, mints, browser int
		out                  string
	}{
		{"identity sp", "sp", "", true, true, 0, 1, 0, "dbx-sp · service principal sp-client · token minted"},
		{"--as user on an sp row", "sp", "user", true, true, 0, 0, 1, "logged in"},
		{"auto with a paired sp mints", "auto", "", true, true, 0, 1, 0, "dbx-sp · service principal sp-client · token minted"},
		{"auto without the sp secret keeps the browser", "auto", "", true, false, 0, 0, 1, "logged in"},
		{"--as sp, unpaired", "user", "sp", false, false, 1, 0, 0, "`claude-code configure databricks` → Identity"},
		{"--as sp, no secret", "auto", "sp", true, false, 1, 0, 0, "`claude-code vault set DATABRICKS_DBX_SP_M2M`"},
	} {
		mints := spLoginRow(t, c.identity, c.paired, c.secret)
		stubValid(t, false)
		browser := 0
		fakeLogin(t, func(*providers.Provider) (time.Duration, error) { browser++; return 30 * time.Minute, nil })
		args := []string{"-p", "dbx-sp"}
		if c.as != "" {
			args = append(args, "--as", c.as)
		}
		code, stdout, stderr := captureOutput(t, func() int { return login(args) })
		out := stdout + stderr
		if code != c.code || int(mints.Load()) != c.mints || browser != c.browser || !strings.Contains(out, c.out) ||
			strings.Contains(out, "sp-secret-value") || strings.Contains(out, "sp-access-0001") {
			t.Errorf("%s: exit %d mints %d browser %d, output %q; want exit %d mints %d browser %d containing %q",
				c.name, code, mints.Load(), browser, out, c.code, c.mints, c.browser, c.out)
		}
	}
}

// A rotation by another process before the row write fails must survive the rollback.
func TestDbxSaveIdentityKeepsConcurrentRotationOnRowFailure(t *testing.T) {
	seedOverlay(t, presetOnly)
	reg := mustLoad(t)
	name := providers.SecretName(providers.GroupDatabricks, "aa", providers.AuthM2M)
	orig := SetRow
	SetRow = func(string, map[string]any) error {
		if err := vault.Store(name, "sp-secret-ROTATED"); err != nil {
			t.Fatal(err)
		}
		return errors.New("overlay write failed")
	}
	t.Cleanup(func() { SetRow = orig })

	if err := SaveIdentity(reg, "aa", providers.IdentitySP, "sp-client-2", "sp-secret-NEW"); err == nil {
		t.Fatal("want the row-write error")
	}
	if got, _, _ := vault.ReadSecret(name); got != "sp-secret-ROTATED" {
		t.Errorf("rollback clobbered the newer secret, stored value length %d", len(got))
	}
}

// --port, --no-browser and --paste reach the oauth options: exact port, no Open, the injected stdin.
func TestLoginHeadlessFlagsReachOptions(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, false)
	in := strings.NewReader("")
	oldIn := loginIn
	loginIn = in
	t.Cleanup(func() { loginIn = oldIn })
	var got oauth.LoginOptions
	old := loginU2M
	loginU2M = func(_ context.Context, _ *providers.Provider, o oauth.LoginOptions) (time.Duration, error) {
		got = o
		return time.Minute, nil
	}
	t.Cleanup(func() { loginU2M = old })
	args := []string{"-p", "dbx-u2m", "--port", "8123", "--no-browser", "--paste"}
	if code, _, stderr := captureOutput(t, func() int { return login(args) }); code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if got.Port != 8123 || !got.ExactPort || !got.NoBrowser || got.Open != nil || got.Paste != io.Reader(in) {
		t.Errorf("options = %+v", got)
	}
}

// The three flags are U2M-only: --as sp and an identity-sp row refuse them (exit 2) and mint nothing.
func TestLoginHeadlessFlagsRejectedForSP(t *testing.T) {
	mints := spLoginRow(t, "sp", true, true)
	for _, args := range [][]string{
		{"-p", "dbx-sp", "--as", "sp", "--port", "8020"},
		{"-p", "dbx-sp", "--no-browser"},
	} {
		code, _, stderr := captureOutput(t, func() int { return login(args) })
		if code != 2 || !strings.Contains(stderr, "browser login only") {
			t.Errorf("%v: exit %d stderr %q, want 2 and the U2M-only message", args, code, stderr)
		}
	}
	if mints.Load() != 0 {
		t.Errorf("mints = %d, want 0", mints.Load())
	}
}

// --dry-run reports the port and modes and opens nothing.
func TestLoginHeadlessDryRunReportsModes(t *testing.T) {
	seedU2MRow(t)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 0, nil })
	code, stdout, _ := captureOutput(t, func() int {
		return login([]string{"-p", "dbx-u2m", "--dry-run", "--force", "--port", "8123", "--no-browser", "--paste"})
	})
	if code != 0 || calls != 0 || !strings.Contains(stdout, "callback port 8123 (exact") ||
		!strings.Contains(stdout, "browser off; paste on") {
		t.Errorf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}
