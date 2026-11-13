package configure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain runs on the hermetic host; the roster rows discovery needs are linked in by this package's imports.
func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	hosttest.Cleanup()
	os.Exit(code)
}

func TestRunMintUsesInstalledVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	const token = "sk-ant-oat01-abcdefghijklmnopqrstuv"
	pathstest.FakeVendor(t, "claude", "printf '%s\\n' '"+token+"'")
	got, err := runMintContext(context.Background(), []string{"claude", "setup-token"})
	if err != nil || got != token {
		t.Fatalf("mint = %q, %v; want installed vendor's token", got, err)
	}
}
func TestVendorArgv(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini", "agy"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			bin := pathstest.FakeVendor(t, name, "exit 0")
			argv := []string{name, "login", "argument with spaces"}
			original := slices.Clone(argv)
			got, err := cliArgv(argv)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{bin, "login", "argument with spaces"}; !slices.Equal(got, want) {
				t.Errorf("cliArgv = %v, want %v", got, want)
			}
			if !slices.Equal(argv, original) {
				t.Errorf("input changed to %v, want %v", argv, original)
			}
			got[1] = "changed"
			if !slices.Equal(argv, original) {
				t.Errorf("result aliases input: %v", argv)
			}
		})
	}
}

func TestVendorArgvGithubUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"gh", "auth", "login"}
	got, err := cliArgv(argv)
	if err != nil || !slices.Equal(got, argv) {
		t.Errorf("cliArgv = %v, %v; want %v, nil", got, err, argv)
	}
}

func TestVendorArgvMissingVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"copilot", "login"}
	original := slices.Clone(argv)
	_, wantErr := paths.AgentBin("copilot")
	if wantErr == nil {
		t.Fatal("expected missing vendor")
	}
	got, err := cliArgv(argv)
	if got != nil || err == nil || err.Error() != wantErr.Error() {
		t.Errorf("cliArgv = %v, %v; want nil, %v", got, err, wantErr)
	}
	if !slices.Equal(argv, original) {
		t.Errorf("input changed to %v, want %v", argv, original)
	}
}

const configureTestToken = "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789"

// configureFakeClaude logs argv, then acts on CONFIGURE_COMMAND_MODE (exists: the first add reports a duplicate until remove runs); `exec sleep` leaves SIGINT nothing to defer.
const configureFakeClaude = `printf '%s\n' "$@" '' >>"$CONFIGURE_COMMAND_LOG"
action=$1
if [ "$1" = mcp ] && [ $# -gt 1 ]; then action=$2; fi
mode=$CONFIGURE_COMMAND_MODE
if [ "$mode" = cancel ] || [ "$mode" = "cancel-$action" ]; then
	: >"$CONFIGURE_COMMAND_READY"
	exec "$CONFIGURE_SLEEP" 30
fi
case $action in
login) echo 'login complete' ;;
mint) printf 'minting token\n` + configureTestToken + `\nfinished\n' ;;
esac
case $mode in exists|exists-remove-fail)
	if [ "$action" = remove ]; then
		: >"$CONFIGURE_COMMAND_LOG.removed"
		if [ "$mode" = exists-remove-fail ]; then echo 'remove failed'; exit 7; fi
	fi
	if [ "$action" = add ] && [ ! -e "$CONFIGURE_COMMAND_LOG.removed" ]; then
		echo 'MCP server web_search already exists'
		exit 1
	fi ;;
esac
if [ "$mode" = fail ] || [ "$mode" = "$action-fail" ]; then
	echo 'command failed'
	exit 7
fi`

func configureCommandEnv(t *testing.T, mode string) (bin, log, ready string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	bin = pathstest.FakeVendor(t, "claude", configureFakeClaude)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	log, ready = filepath.Join(dir, "argv"), filepath.Join(dir, "ready")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CONFIGURE_SLEEP", sleep)
	t.Setenv("CONFIGURE_COMMAND_MODE", mode)
	t.Setenv("CONFIGURE_COMMAND_LOG", log)
	t.Setenv("CONFIGURE_COMMAND_READY", ready)
	return bin, log, ready
}

func captureConfigureStdout(t *testing.T, run func()) string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close stdout capture: %v", err)
		}
	})
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }()
	run()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func assertConfigureArgv(t *testing.T, log string, commands ...[]string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, args := range commands {
		want.WriteString(strings.Join(args, "\n"))
		want.WriteString("\n\n")
	}
	if string(raw) != want.String() {
		t.Errorf("command argv = %q, want %q", raw, want.String())
	}
}

func assertConfigureCancellation(t *testing.T, ready string, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitConfigureReady(t, ctx, ready, done)
	canceledAt := time.Now()
	cancel()
	select {
	case err := <-done:
		// under WaitDelay (3s): SIGINT ended the child, not the kill fallback
		if d := time.Since(canceledAt); d > 2500*time.Millisecond {
			t.Errorf("cancellation took %v, want prompt SIGINT exit (< 2.5s, far under the 30s helper sleep)", d)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled command error = %v, want wrapped context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not return promptly after cancellation")
	}
}

// waitConfigureReady polls until the helper creates ready, failing if the command ends or ctx expires first.
func waitConfigureReady(t *testing.T, ctx context.Context, ready string, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(ready)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("command ended before cancellation: %v", err)
		case <-ctx.Done():
			t.Fatal("helper did not become ready before the deadline")
		case <-ticker.C:
		}
	}
}

func TestLoginAndMintContext(t *testing.T) {
	for _, mint := range []bool{false, true} {
		action := "login"
		if mint {
			action = "mint"
		}
		for _, mode := range []string{"normal", "fail", "cancel"} {
			t.Run(action+"/"+mode, func(t *testing.T) { runLoginMintCase(t, action, mode) })
		}
	}
}

// runLoginMintCase runs the fake claude's login or mint action in mode (normal, fail, cancel) and checks the outcome.
func runLoginMintCase(t *testing.T, action, mode string) {
	mint := action == "mint"
	bin, log, ready := configureCommandEnv(t, mode)
	argv := []string{bin, action, "argument with spaces"}
	if !mint {
		argv[0] = "claude"
	}
	var token string
	run := func(ctx context.Context) error {
		if !mint {
			return runAttached(ctx, "login", argv, os.Stdout)
		}
		var err error
		token, err = runMintContext(ctx, argv)
		return err
	}
	if mode == "cancel" {
		assertConfigureCancellation(t, ready, run)
		assertConfigureArgv(t, log, argv[1:])
		return
	}
	var err error
	out := captureConfigureStdout(t, func() { err = run(context.Background()) })
	if mode == "fail" {
		assertCommandFailed(t, err, token)
	} else {
		assertCommandSucceeded(t, err, out, token, mint)
	}
	assertConfigureArgv(t, log, argv[1:])
}

// assertCommandFailed requires the fake command's exit status 7 and no minted token.
func assertCommandFailed(t *testing.T, err error, token string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Errorf("command error = %v, want exit status 7", err)
	}
	if token != "" {
		t.Errorf("failed mint returned token %q", token)
	}
}

// assertCommandSucceeded requires a clean run whose stdout (and, for a mint, token) match the fake's script.
func assertCommandSucceeded(t *testing.T, err error, out, token string, mint bool) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	wantOut := "login complete\n"
	if mint {
		wantOut = "minting token\n" + configureTestToken + "\nfinished\n"
		if token != configureTestToken {
			t.Errorf("minted token = %q, want %q", token, configureTestToken)
		}
	}
	if out != wantOut {
		t.Errorf("stdout = %q, want %q", out, wantOut)
	}
}

// assertMaxPlanOptions requires the Max-plan row's three ways in: login, mint, paste.
func assertMaxPlanOptions(t *testing.T, opts []AuthOption) {
	t.Helper()
	if len(opts) != 3 || opts[0].Argv == nil || opts[1].Mint == nil || opts[1].Env != "CLAUDE_CODE_OAUTH_TOKEN" || opts[2].Env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("authOptions = %+v, want login, mint, paste", opts)
	}
}

// The Max-plan row offers three ways in: browser login, minting a headless token, pasting one; the minted token
// is recognised in the CLI's output by its sk-ant-oat prefix.
func TestMaxPlanOptionsAndSetupTokenCapture(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: status reads as not logged in
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	kt := maxPlanTarget(KeyTarget{Label: "claude"})
	if kt.Present || kt.Headless {
		t.Fatalf("no login and no token must read absent: %+v", kt)
	}
	assertMaxPlanOptions(t, AuthOptions(kt))
	out := "Opening browser...\nPaste the code here:\n\nYour token: sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789\n"
	if got := setupTokenRE.FindString(out); got != "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789" {
		t.Errorf("token capture = %q", got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored")
	if kt := maxPlanTarget(KeyTarget{Label: "claude"}); !kt.Present || !kt.Headless || !strings.Contains(kt.Source, "headless token") {
		t.Errorf("a stored token must read present/headless: %+v", kt)
	}
}

// copilot present via a raw env token override offers no rotate CLI (`copilot login` would rotate
// copilot's own session, not the override); present via copilot's/gh's own login does.
func TestVendorKeyTargetCopilotRotateWiring(t *testing.T) {
	t.Setenv("PATH", t.TempDir())              // no real gh/secret-tool on PATH
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // CLAUDE_CONFIG_DIR isolated so nothing real leaks in
	t.Setenv("COPILOT_HOME", t.TempDir())      // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir())     // no real gh hosts.yml: GHActiveAccount reads nothing
	t.Setenv("GH_TOKEN", "ghp_env_override")
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := AgentKeyTarget(reg, "copilot")
	if !kt.Present || len(kt.LoginArgv) != 0 {
		t.Errorf("env-token copilot = %+v, want present with no loginArgv", kt)
	}
}

// codex present via its own ChatGPT login (file-based, no codex binary needed) offers `codex login` as the
// rotate CLI.
func TestVendorKeyTargetCodexRotateWiring(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := AgentKeyTarget(reg, "codex")
	if !kt.Present {
		t.Fatalf("codex = %+v, want present via the ChatGPT login file", kt)
	}
	if len(kt.LoginArgv) != 2 || kt.LoginArgv[0] != "codex" || kt.LoginArgv[1] != "login" {
		t.Errorf("codex loginArgv = %v, want [codex login]", kt.LoginArgv)
	}
}

// gemini present via agy's cached Google login (~/.gemini/antigravity-cli/antigravity-oauth-token) offers
// `agy` as the rotate CLI.
func TestVendorKeyTargetGeminiGoogleLoginRotateWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := AgentKeyTarget(reg, "gemini")
	if !kt.Present {
		t.Fatalf("gemini = %+v, want present via the agy login marker", kt)
	}
	if len(kt.LoginArgv) != 1 || kt.LoginArgv[0] != "agy" {
		t.Errorf("gemini loginArgv = %v, want [agy]", kt.LoginArgv)
	}
}

// the switch flipping off -> on warms the cache even though no pin changed.
func TestWarmServedOnFlipOn(t *testing.T) {
	n := servedHarness(t, true)
	vaulttest.SetRecord(t, "claude", "profile", "qa")
	var out bytes.Buffer
	WarmServedOnChange(&out, false, testRoutingPins(t), false)
	if *n == 0 || !cached(t) {
		t.Fatalf("flip-on made %d requests, cached=%v, want a warmed cache", *n, cached(t))
	}
}

// a pin change with the switch already on warms; the same pins with it already on do not.
func TestWarmServedOnPinChangeOnly(t *testing.T) {
	n := servedHarness(t, true)
	before := testRoutingPins(t)
	var out bytes.Buffer
	WarmServedOnChange(&out, false, before, true)
	if *n != 0 {
		t.Fatalf("unchanged pins and switch warmed: %d requests", *n)
	}
	vaulttest.SetRecord(t, "claude", "profile", "qa")
	WarmServedOnChange(&out, false, before, true)
	if *n == 0 || !cached(t) {
		t.Fatalf("pin change made %d requests, cached=%v, want a warmed cache", *n, cached(t))
	}
}

// with the switch off, even a pin change makes no request.
func TestWarmServedSwitchOffNeverWarms(t *testing.T) {
	n := servedHarness(t, false)
	before := testRoutingPins(t)
	vaulttest.SetRecord(t, "claude", "profile", "qa")
	WarmServedOnChange(&bytes.Buffer{}, false, before, false)
	if *n != 0 || cached(t) {
		t.Fatalf("switch off: %d requests, cached=%v", *n, cached(t))
	}
}

// --check and --dry-run never warm, whatever changed.
func TestWarmServedSkippedForCheckAndDryRun(t *testing.T) {
	n := servedHarness(t, true)
	before := testRoutingPins(t)
	vaulttest.SetRecord(t, "claude", "profile", "qa")
	WarmServedOnChange(&bytes.Buffer{}, true, before, false)
	if *n != 0 || cached(t) {
		t.Fatalf("skip=true: %d requests, cached=%v", *n, cached(t))
	}
}

// servedHarness seeds a hermetic vault with a Databricks row "qa" (served-aware routing set to on) pinned as the
// session profile, and a TLS listing server; it returns the request counter.
func servedHarness(t *testing.T, on bool) *int {
	t.Helper()
	vaulttest.Use(t)
	var n int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/scim/v2/Me") { // the identity check, not a listing request
			_, _ = w.Write([]byte(`{"id":"u1","active":true}`))
			return
		}
		n++
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"m-a","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, srv.URL))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-qa")
	return &n
}

func cached(t *testing.T) bool {
	t.Helper()
	_, ok := router.LoadServed("qa")
	return ok
}

func testRoutingPins(t *testing.T) map[string]router.PinSpec {
	t.Helper()
	pins, err := agent.AgentPins()
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

// fakeKeyedProvider is a provider whose catalog route is a local server; want is the only candidate the
// server accepts, so probeValue/ProbeCredential drive "verified" vs "rejected" without a real network call.
func fakeKeyedProvider(t *testing.T, envVar, want string) *providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return &providers.Provider{
		Name: "fake", Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar},
	}
}

func TestRotateOptionsShapes(t *testing.T) {
	cases := []struct {
		name string
		kt   KeyTarget
		want []string
	}{
		{"login only", KeyTarget{LoginArgv: []string{"codex", "login"}}, []string{"Login"}},
		{"mint only", KeyTarget{MintArgv: []string{"claude", "setup-token"}}, []string{"OAuth token"}},
		{"login and mint (subscription)", KeyTarget{LoginArgv: []string{"claude", "auth", "login"}, MintArgv: []string{"claude", "setup-token"}},
			[]string{"Login", "OAuth token"}},
		{"keyed, no generator", KeyTarget{EnvVar: "X", Provider: &providers.Provider{}}, []string{"API key"}},
		{"keyed but no provider: no generator and no paste", KeyTarget{EnvVar: "X"}, nil},
		{"nothing at all", KeyTarget{}, nil},
	}
	for _, c := range cases {
		opts := RotateOptions(c.kt)
		var got []string
		for _, o := range opts {
			got = append(got, o.Action)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: rotateOptions = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: rotateOptions = %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestParseAssignment(t *testing.T) {
	cases := []struct {
		raw, name, value string
	}{
		{"sk-bare-value", "", "sk-bare-value"},
		{"NAME=v", "NAME", "v"},
		{`export NAME="v"`, "NAME", "v"},
		{"NAME='v'", "NAME", "v"},
		{"abc==", "", "abc=="},   // base64 padding stays a bare value, never split as NAME=value
		{"ABC=", "", "ABC="},     // trailing '=' with no value: Cut's v=="" rule keeps it bare
		{"name=v", "", "name=v"}, // lowercase "key" is not an env-var-shaped name: stays bare
	}
	for _, c := range cases {
		name, value := ParseAssignment(c.raw)
		if name != c.name || value != c.value {
			t.Errorf("ParseAssignment(%q) = (%q, %q), want (%q, %q)", c.raw, name, value, c.name, c.value)
		}
	}
}

// The list is harness-read names plus the user's own: a console-URL key no enabled provider reads is not a row,
// an indexed custom secret is.
func TestTrackedSecretsListsHarnessAndCustomOnly(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers": {"openai": null}}`)
	if err := vault.Store("MY_CUSTOM", "v"); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, s := range TrackedSecrets(reg) {
		rows[s.Name] = s.Users
	}
	if _, ok := rows["OPENAI_API_KEY"]; ok {
		t.Error("OPENAI_API_KEY listed although its provider is disabled")
	}
	if rows["MY_CUSTOM"] != "custom" || rows[providers.OAuthTokenEnv] == "" {
		t.Errorf("rows = %v, want MY_CUSTOM (custom) and the setup-token", rows)
	}
}

func TestStoreSecretAndStoreImportClearAuthMarks(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{
		"clr-env":{"protocol":"anthropic","base_url":"https://e.example","auth":{"type":"key","header":"x-api-key","env":"CLR_ENV_KEY"},"models":{"opus":"m"}},
		"clr-other":{"protocol":"anthropic","base_url":"https://o.example","auth":{"type":"key","header":"x-api-key","env":"CLR_OTHER_KEY"},"models":{"opus":"m"}}}}}`)
	target, unrelated := router.ProviderAccount("clr-env"), router.ProviderAccount("clr-other")
	for _, account := range []string{target, unrelated} {
		if err := router.MarkAuth(account, "auth rejected"); err != nil {
			t.Fatal(err)
		}
	}
	isMarked := func(account string) bool {
		_, ok := router.LoadState().AuthMarked(account, time.Now())
		return ok
	}

	if err := StoreSecret("CLR_ENV_KEY", "stored-secret-value"); err != nil {
		t.Fatal(err)
	}
	if isMarked(target) || !isMarked(unrelated) {
		t.Fatalf("after StoreSecret: target=%v unrelated=%v, want false/true", isMarked(target), isMarked(unrelated))
	}
	if err := router.MarkAuth(target, "auth rejected"); err != nil {
		t.Fatal(err)
	}
	if err := StoreImport([]vault.Pair{{Name: "CLR_ENV_KEY", Value: "imported-secret-value"}}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if isMarked(target) || !isMarked(unrelated) {
		t.Errorf("after StoreImport: target=%v unrelated=%v, want false/true", isMarked(target), isMarked(unrelated))
	}
}

func TestProbeCachedVaultAndEnvOverride(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_CACHED_SECRET"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	meta, err := vault.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.RecordProbe(name, "verified", time.Now().Add(-time.Minute), meta[name].SetAt); err != nil {
		t.Fatal(err)
	}
	p := fakeKeyedProvider(t, name, "env-value")
	kt := KeyTarget{Provider: p}
	status, _, age := ProbeCached(kt)
	if status != "verified" || age < time.Minute {
		t.Fatalf("vault cache = (%q, %v), want verified cached result", status, age)
	}
	t.Setenv(name, "env-value")
	status, _, age = ProbeCached(kt)
	if status != "verified" || age != 0 {
		t.Fatalf("env override = (%q, %v), want verified live result", status, age)
	}
}

func TestProbeValueRejectsInvalidCandidateBeforeRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const name = "CONFIGURE_PROBE_INVALID"
	const candidate = "private-invalid\x00candidate"
	t.Setenv(name, "prior-good-credential")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write catalog: %v", err)
		}
	}))
	defer srv.Close()
	p := &providers.Provider{
		Name: "invalid-candidate", Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: name},
	}
	status, detail := ProbeValue(p, name, candidate)
	if status != "unreachable" || detail == "" {
		t.Errorf("probeValue = (%q, %q), want unreachable with an error detail", status, detail)
	}
	for _, raw := range []string{candidate, "private-invalid", "prior-good-credential"} {
		if strings.Contains(detail, raw) {
			t.Errorf("probe detail leaked a credential: %q", detail)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("invalid candidate made %d HTTP requests with a prior credential", got)
	}
	if got := os.Getenv(name); got != "prior-good-credential" {
		t.Errorf("probe changed original credential to %q", got)
	}
}

// envEverSet polls name until the returned stop func is called; stop reports whether it was ever set.
func envEverSet(name string) (stop func() bool) {
	done, finished := make(chan struct{}), make(chan struct{})
	var seen atomic.Bool
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
				if _, set := os.LookupEnv(name); set {
					seen.Store(true)
				}
			}
		}
	}()
	return func() bool { close(done); <-finished; return seen.Load() }
}

// probeIsolated probes prov with cand: verified when own, else rejected; no detail may carry a candidate.
func probeIsolated(prov *providers.Provider, name, cand string, own bool, all []string) bool {
	status, detail := ProbeValue(prov, name, cand)
	want := providers.ProbeRejected
	if own {
		want = providers.ProbeVerified
	}
	return status == want && !slices.ContainsFunc(all, func(c string) bool { return strings.Contains(detail, c) })
}

func TestProbeValueConcurrentCandidatesStayIsolated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const name = "CONFIGURE_PROBE_CONCURRENT"
	if _, set := os.LookupEnv(name); set {
		t.Fatalf("%s is set in the test environment", name)
	}
	cands := []string{"candidate-alpha-9f1c", "candidate-beta-27d4"}
	provs := []*providers.Provider{fakeKeyedProvider(t, name, cands[0]), fakeKeyedProvider(t, name, cands[1])}
	stop := envEverSet(name)
	bad := probeAllConcurrently(provs, name, cands)
	if stop() {
		t.Errorf("%s was set in the process environment during a probe", name)
	}
	if bad != 0 {
		t.Errorf("%d probes saw another probe's candidate or leaked one in the detail (mismatch)", bad)
	}
}

// probeAllConcurrently probes each provider with its own and the other candidate, 20 rounds; it returns mismatches.
func probeAllConcurrently(provs []*providers.Provider, name string, cands []string) int32 {
	var bad atomic.Int32
	done := make(chan struct{})
	launched := 0
	for range 20 {
		for i := range cands {
			for _, own := range []bool{true, false} {
				launched++
				go func() {
					defer func() { done <- struct{}{} }()
					cand := cands[i]
					if !own {
						cand = cands[1-i]
					}
					if !probeIsolated(provs[i], name, cand, own, cands) {
						bad.Add(1)
					}
				}()
			}
		}
	}
	for range launched {
		<-done
	}
	return bad.Load()
}

// Dropping tailscale from `configure keys`/secrets is deliberate (BREAKING CHANGE): no row, no
// TAILSCALE_OAUTH_CLIENT_SECRET console link — both now live only in the read-only remote section.
func TestKeysAndSecretsNeverCarryTailscale(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	all, err := KeyTargets(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasLabel(all, "tailscale") {
		t.Error(`keyTargets("") must no longer carry a tailscale row (moved to the remote section)`)
	}
	if ConsoleURLs["TAILSCALE_OAUTH_CLIENT_SECRET"] != "" {
		t.Error("consoleURLs must no longer carry TAILSCALE_OAUTH_CLIENT_SECRET (configure keys no longer prompts for it)")
	}
}

func hasLabel(ts []KeyTarget, label string) bool {
	for _, kt := range ts {
		if kt.Label == label {
			return true
		}
	}
	return false
}
