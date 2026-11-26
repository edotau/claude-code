package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m", "--dry-run"}) })
	if code != 0 || !strings.Contains(stdout, "DATABRICKS_DBX_U2M_OAUTH") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestVaultLoginRefusesRowWithAnotherAuthType(t *testing.T) {
	vaulttest.UseFreshKey(t)
	err := providers.SetProviderFields("dbx-pat", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-pat", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "--auth-type oauth") ||
		!strings.Contains(stderr, "without a browser") || !strings.Contains(stderr, "login -p <oauth profile>") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

// Commands take flags only: a bare row name is a usage error naming --profile.
func TestLoginAndMintRefusePositionalRow(t *testing.T) {
	for _, run := range []func([]string) int{login, mint} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"dev"}) })
		if code != 2 || !strings.Contains(stderr, "pass --profile dev") {
			t.Fatalf("exit %d stderr %q, want 2 and a --profile hint", code, stderr)
		}
	}
}

// -p is --profile's shorthand on both commands.
func TestLoginAndMintTakeShorthandP(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // scratch vault: never the real overlay
	for _, run := range []func([]string) int{login, mint} {
		_, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"-p", "no-such-row", "--dry-run"}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return login([]string{"--profile", "dbx-dev", "--auth-type", "oauth"})
	})
	if code == 0 || !strings.Contains(stderr, "providers.prev") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

// -t is --auth-type's shorthand, and it counts as an explicit switch like the long form.
func TestLoginShorthandT(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "x", "-t", "m2m"}) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m"}) })
	if code != 0 || calls != 0 || !strings.Contains(stdout, "already logged in · expires in 40m") {
		t.Fatalf("exit %d calls %d stdout %q stderr %q", code, calls, stdout, stderr)
	}
}

func TestLoginSuccessClearsAuthMark(t *testing.T) {
	seedU2MRow(t)
	for _, valid := range []bool{true, false} {
		stubValid(t, valid)
		calls := 0
		fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return time.Minute, nil })
		if err := router.MarkAuth(router.ProviderAccount("dbx-u2m"), "login rejected (401)"); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m"}) })
		if code != 0 || calls != 1 || strings.Contains(stdout, "already") {
			t.Fatalf("valid=%v exit %d calls %d stdout %q stderr %q", valid, code, calls, stdout, stderr)
		}
		if _, ok := router.LoadState().AuthMarked(router.ProviderAccount("dbx-u2m"), time.Now()); ok {
			t.Errorf("valid=%v: auth mark survived a successful login", valid)
		}
	}
}

func TestLoginExpiredTokenOpensLogin(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, false)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m"}) })
	if code != 0 || calls != 1 || !strings.Contains(stdout, "logged in · token expires in 30m") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

func TestLoginForceSkipsReuse(t *testing.T) {
	seedU2MRow(t)
	stubValid(t, true)
	calls := 0
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { calls++; return 30 * time.Minute, nil })
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"--profile", "dbx-u2m", "--force"}) })
	if code != 0 || calls != 1 || strings.Contains(stdout, "already") {
		t.Fatalf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

// With no Databricks profile, login and mint name the import command instead of `unknown profile ""`.
func TestNoDatabricksProfileNamesImport(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for name, run := range map[string]func([]string) int{"login": login, "mint": mint} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"--dry-run"}) })
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
		hosttest.CaptureOutput(t, func() int { code, bad = checkLoginFlags(as, ""); return 0 })
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
		{"--as sp, no secret", "auto", "sp", true, false, 1, 0, 0, "`claude-code vault set secrets DATABRICKS_DBX_SP_M2M`"},
	} {
		mints := spLoginRow(t, c.identity, c.paired, c.secret)
		stubValid(t, false)
		browser := 0
		fakeLogin(t, func(*providers.Provider) (time.Duration, error) { browser++; return 30 * time.Minute, nil })
		args := []string{"-p", "dbx-sp"}
		if c.as != "" {
			args = append(args, "--as", c.as)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return login(args) })
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

// A rollback that deletes the secret it created writes the secret_delete audit line vault.Delete would.
func TestDbxSaveIdentityRollbackDeleteIsAudited(t *testing.T) {
	seedOverlay(t, presetOnly)
	reg := mustLoad(t)
	name := providers.SecretName(providers.GroupDatabricks, "aa", providers.AuthM2M)
	orig := SetRow
	SetRow = func(string, map[string]any) error { return errors.New("overlay write failed") }
	t.Cleanup(func() { SetRow = orig })
	if err := SaveIdentity(reg, "aa", providers.IdentitySP, "sp-client-2", "sp-secret-NEW"); err == nil {
		t.Fatal("want the row-write error")
	}
	if _, found, err := vault.ReadSecret(name); err != nil || found {
		t.Fatalf("rollback kept the secret: found=%v err=%v", found, err)
	}
	if got := auditedDeletes(t); !slices.Equal(got, []string{name}) {
		t.Fatalf("audited deletes %v, want [%s]", got, name)
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return login(args) }); code != 0 {
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
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return login(args) })
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
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		return login([]string{"-p", "dbx-u2m", "--dry-run", "--force", "--port", "8123", "--no-browser", "--paste"})
	})
	if code != 0 || calls != 0 || !strings.Contains(stdout, "callback port 8123 (exact") ||
		!strings.Contains(stdout, "browser off; paste on") {
		t.Errorf("exit %d calls %d stdout %q", code, calls, stdout)
	}
}

func TestSwitchAuthTypeM2MNeedsClientID(t *testing.T) {
	mintRow(t, 200)
	before := overlayDoc(t)
	p := &providers.Provider{Name: "mrow"}
	if err := switchAuthType(p, providers.AuthM2M, false); err == nil || !strings.Contains(err.Error(), "needs auth.clientId") {
		t.Fatalf("err %v", err)
	}
	if overlayDoc(t) != before {
		t.Error("failed switch wrote the overlay")
	}
	seedOverlay(t, `{"providers":{"databricks":{"mrow":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","command":"true","type":"m2m","clientId":"cid"}}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, _ = reg.Get("mrow")
	if err := switchAuthType(p, providers.AuthM2M, true); err != nil {
		t.Errorf("with clientId: %v", err)
	}
	if err := switchAuthType(p, "bogus", true); err == nil {
		t.Error("unsupported authType accepted")
	}
}

func TestSwitchAuthTypeDiffAndRollbackHintUnchanged(t *testing.T) {
	seedCLIProfileRow(t)
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { return 0, context.DeadlineExceeded })
	var out strings.Builder
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		if err := LoginRow(context.Background(), "dbx-dev", &out); err == nil {
			return 0
		}
		return 1
	})
	if code != 1 || out.Len() != 0 {
		t.Fatalf("exit %d out %q", code, out.String())
	}
	for _, want := range []string{`"type": "oauth"`, "kept custom auth.refreshCommand"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff missing %q:\n%s", want, stdout)
		}
	}
	if want := "auth.type of dbx-dev is now oauth (was \"key\")"; !strings.Contains(stderr, want) {
		t.Errorf("rollback hint missing %q:\n%s", want, stderr)
	}
}

func TestLoginRowSuccessLineNoRollback(t *testing.T) {
	seedCLIProfileRow(t)
	fakeLogin(t, func(*providers.Provider) (time.Duration, error) { return 25 * time.Minute, nil })
	var out strings.Builder
	_, _, stderr := hosttest.CaptureOutput(t, func() int {
		if err := LoginRow(context.Background(), "dbx-dev", &out); err != nil {
			t.Error(err)
		}
		return 0
	})
	if out.String() != "logged in · token expires in 25m\n" {
		t.Errorf("out %q", out.String())
	}
	if strings.Contains(stderr, "providers.prev") {
		t.Errorf("rollback hint on success: %q", stderr)
	}
}

const remoteHost = "devhost.example.ts.net"

// remoteWorld is a Databricks profile with a paired service principal on a fake token endpoint, ssh calls and opener.
type remoteWorld struct {
	codes, mints, opensLocal atomic.Int32
	forwards                 [][4]any // host, first, last, local of each ReverseForward
	stops                    atomic.Int32
	hostPort                 func(local int) int // the host port the fake forward lands on
	forwardErr               error
	opened                   []string // URLs the host was asked to open
	redirect                 string   // redirect_uri of the URL the host browser got
	localPort                int
}

func newRemoteWorld(t *testing.T) *remoteWorld {
	t.Helper()
	w := &remoteWorld{hostPort: func(local int) int { return local }}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rw.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("grant_type") == "client_credentials" {
			w.mints.Add(1)
			fmt.Fprint(rw, `{"access_token":"sp-access-0001","expires_in":3600}`)
			return
		}
		w.codes.Add(1)
		if r.PostForm.Get("redirect_uri") != w.redirect {
			http.Error(rw, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		fmt.Fprint(rw, `{"access_token":"user-access-0001","refresh_token":"user-refresh-0001","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	seedOverlay(t, fmt.Sprintf(`{"providers":{"databricks":{"dbx-rem":{"protocol":"anthropic","base_url":%q,`+
		`"auth":{"type":"oauth","spClientId":"sp-client"},"identity":"auto"}}}}`, srv.URL))
	if err := vault.Store("DATABRICKS_DBX_REM_M2M", "sp-secret-value"); err != nil {
		t.Fatal(err)
	}
	stubValid(t, false)
	t.Setenv(remote.LoginRemoteEnv, remoteHost)
	w.swapSeams(t)
	return w
}

func (w *remoteWorld) swapSeams(t *testing.T) {
	t.Helper()
	oldLocal, oldFwd, oldOpen, oldWait := openLocal, forwardPort, openOnHost, loginDeadline
	t.Cleanup(func() { openLocal, forwardPort, openOnHost, loginDeadline = oldLocal, oldFwd, oldOpen, oldWait })
	loginDeadline = 3 * time.Second // a login that never gets its callback fails the test instead of hanging it
	openLocal = func(string) error { w.opensLocal.Add(1); return errors.New("a browser was opened on this machine") }
	forwardPort = func(_ context.Context, host string, first, last, local int) (int, func(), error) {
		w.forwards = append(w.forwards, [4]any{host, first, last, local})
		if w.forwardErr != nil {
			return 0, nil, w.forwardErr
		}
		w.localPort = local
		return w.hostPort(local), func() { w.stops.Add(1) }, nil
	}
	openOnHost = func(_ context.Context, host, authURL string) error {
		w.opened = append(w.opened, host+" "+authURL)
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		w.redirect = q.Get("redirect_uri")
		// The host's browser lands on localhost:<redirect port>, which the forward carries to this machine's listener.
		cb := "http://127.0.0.1:" + strconv.Itoa(w.localPort) + "/?code=the-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(cb)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
}

func (w *remoteWorld) sealed(t *testing.T) bool {
	t.Helper()
	return providers.HasSecret("DATABRICKS_DBX_REM_OAUTH") != ""
}

func TestRemoteLoginSignsInOnHostAndSealsHere(t *testing.T) {
	for _, c := range []struct {
		name     string
		hostPort func(int) int
	}{
		{"same port", func(l int) int { return l }},
		{"port taken on the host, next one used", func(l int) int { return l + 1 }},
	} {
		w := newRemoteWorld(t)
		w.hostPort = c.hostPort
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
		if code != 0 || w.codes.Load() != 1 || w.mints.Load() != 0 || !w.sealed(t) || w.opensLocal.Load() != 0 {
			t.Fatalf("%s: exit %d codes %d mints %d sealed %v local opens %d stderr %q; want a user sign-in, no mint, sealed here, no local browser",
				c.name, code, w.codes.Load(), w.mints.Load(), w.sealed(t), w.opensLocal.Load(), stderr)
		}
		assertRemoteSignIn(t, c.name, w, stderr)
	}
}

func assertRemoteSignIn(t *testing.T, name string, w *remoteWorld, stderr string) {
	t.Helper()
	if len(w.forwards) != 1 || w.forwards[0][0] != remoteHost || w.forwards[0][2] != 8050 || w.stops.Load() != 1 {
		t.Errorf("%s: forwards %v stops %d; want one forward on %s up to 8050, closed", name, w.forwards, w.stops.Load(), remoteHost)
	}
	if want := "http://localhost:" + strconv.Itoa(w.hostPort(w.localPort)); w.redirect != want {
		t.Errorf("%s: redirect_uri %q, want the host's port %q", name, w.redirect, want)
	}
	if len(w.opened) != 1 || !strings.HasPrefix(w.opened[0], remoteHost+" http") || !strings.Contains(stderr, "/oidc/v1/authorize?") {
		t.Errorf("%s: opened %q, stderr %q; want the host asked once and the URL printed here", name, w.opened, stderr)
	}
}

func TestRemoteLoginExactPortTakenNamesPortAndHost(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = &remote.PortTakenError{Host: remoteHost, First: 8040, Last: 8040}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--port", "8040"}) })
	if code != 1 || !strings.Contains(stderr, "port 8040 on "+remoteHost) || len(w.opened) != 0 || w.sealed(t) {
		t.Errorf("exit %d stderr %q opened %q", code, stderr, w.opened)
	}
	if f := w.forwards[0]; f[1] != 8040 || f[2] != 8040 {
		t.Errorf("forward range %v, want exactly 8040", f)
	}
}

func TestRemoteLoginSSHFailureNamesHostAndPasteAndOpensNoLocalBrowser(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = errors.New("ssh to " + remoteHost + ": Connection refused")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
	if code != 1 || !strings.Contains(stderr, remoteHost) || !strings.Contains(stderr, "--paste") || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Errorf("exit %d stderr %q, want one line naming %s and --paste", code, stderr, remoteHost)
	}
	if w.opensLocal.Load() != 0 || len(w.opened) != 0 || w.sealed(t) {
		t.Errorf("local opens %d, host opens %q, sealed %v; want none", w.opensLocal.Load(), w.opened, w.sealed(t))
	}
}

// With --paste a forward that cannot be made is not an error: the URL is printed and the redirected URL can be pasted.
func TestRemoteForwardFailureWithPasteContinuesWithoutOpening(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = errors.New("ssh to " + remoteHost + ": Connection refused")
	opts := headless{paste: true, via: remoteHost}.remoteOptions()
	port, stop, err := opts.Forward(context.Background(), 8031)
	if err != nil || port != 8031 || stop == nil {
		t.Fatalf("port %d err %v, want the local port and no error", port, err)
	}
	if err := opts.Open("https://x/oidc/v1/authorize"); err == nil || len(w.opened) != 0 || w.opensLocal.Load() != 0 {
		t.Errorf("open err %v, host opens %q, local opens %d; want a refusal and no browser anywhere", err, w.opened, w.opensLocal.Load())
	}
	if opts.Paste == nil {
		t.Error("--paste not carried into the options")
	}
}

func TestRemoteLoginAsSPMintsWithNoForward(t *testing.T) {
	w := newRemoteWorld(t)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--as", "sp"}) })
	if code != 0 || w.mints.Load() != 1 || len(w.forwards) != 0 || len(w.opened) != 0 || w.opensLocal.Load() != 0 ||
		!strings.Contains(stdout, "service principal sp-client · token minted") {
		t.Errorf("exit %d mints %d forwards %v opened %q stdout %q", code, w.mints.Load(), w.forwards, w.opened, stdout)
	}
}

// Without --remote a profile with a paired service principal still mints and opens no browser.
func TestBareLoginWithPairedSPIsUnchanged(t *testing.T) {
	w := newRemoteWorld(t)
	t.Setenv(remote.LoginRemoteEnv, "")
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
	if code != 0 || w.mints.Load() != 1 || w.codes.Load() != 0 || len(w.forwards) != 0 || w.opensLocal.Load() != 0 ||
		!strings.Contains(stdout, "service principal sp-client · token minted") {
		t.Errorf("exit %d mints %d codes %d forwards %v local opens %d stdout %q",
			code, w.mints.Load(), w.codes.Load(), w.forwards, w.opensLocal.Load(), stdout)
	}
}

func TestRemoteLoginRefusesNoBrowser(t *testing.T) {
	w := newRemoteWorld(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--no-browser"}) })
	if code != 2 || !strings.Contains(stderr, "--no-browser contradicts --remote") || len(w.forwards) != 0 {
		t.Errorf("exit %d stderr %q forwards %v", code, stderr, w.forwards)
	}
}

func TestRemoteLoginDryRunSaysWhatWouldHappenAndConnectsNowhere(t *testing.T) {
	w := newRemoteWorld(t)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--dry-run"}) })
	for _, want := range []string{"sign the user in for dbx-rem", "browser on " + remoteHost,
		"callback forwarded from " + remoteHost + ":8030", "to this machine", "sealed in this machine's vault", "no ssh connection made"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if code != 0 || len(w.forwards) != 0 || len(w.opened) != 0 || w.opensLocal.Load() != 0 || w.sealed(t) {
		t.Errorf("exit %d forwards %v opened %q sealed %v; a dry run does nothing", code, w.forwards, w.opened, w.sealed(t))
	}
}

func TestOpenURLVerbOpensOneHTTPSURL(t *testing.T) {
	var got []string
	old := openLocal
	openLocal = func(u string) error { got = append(got, u); return nil }
	t.Cleanup(func() { openLocal = old })
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return openURLVerb([]string{"https://x.example/a?b=1&c=2"}) }); code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, bad := range [][]string{{}, {"http://x.example"}, {"file:///etc/passwd"}, {"-n"}, {"https://a", "https://b"}} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return openURLVerb(bad) }); code != 2 {
			t.Errorf("open-url %q: exit %d, want 2", bad, code)
		}
	}
	if len(got) != 1 || got[0] != "https://x.example/a?b=1&c=2" {
		t.Errorf("opened %q, want the one good URL", got)
	}
}

func TestBrowserLoginDefaultsToFirstRemoteHostButSPMintStaysLocal(t *testing.T) {
	w := newRemoteWorld(t)
	t.Setenv(remote.LoginRemoteEnv, "")
	t.Setenv(providers.RemoteHostEnv, "box-a.example, box-b.example")
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--as", "sp"}) }); code != 0 ||
		w.mints.Load() != 1 || len(w.forwards) != 0 || w.opensLocal.Load() != 0 {
		t.Fatalf("sp mint: code %d mints %d forwards %v local opens %d", code, w.mints.Load(), w.forwards, w.opensLocal.Load())
	}
	code, _, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--as", "user", "--force"}) })
	if code != 0 || len(w.forwards) != 1 || w.forwards[0][0] != "box-a.example" || len(w.opened) != 1 || w.opensLocal.Load() != 0 {
		t.Errorf("user login: code %d forwards %v opened %q local opens %d", code, w.forwards, w.opened, w.opensLocal.Load())
	}
}
