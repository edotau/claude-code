package menu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/ui/ansi"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runKeysCheck drives runKeysChanged directly the way Run's Sections walk used to before `keys` was
// removed from Sections: --check/piped is still read-only here (resolveKey/runAuthOption never fire
// for an absent key when o.Check or !o.Interactive), so this reproduces the old `Run(opts, "keys")`
// behavior without going through the removed section name. !allOK maps to the CLI's old rc 1.
func runKeysCheck(t *testing.T, o Options) (out bytes.Buffer, rc int) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load: %v", err)
	}
	o.In = o.reader()
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: &out}
	d.header("keys")
	allOK, _ := runKeysChanged(d)
	if !allOK {
		rc = 1
	}
	return out, rc
}

// A piped --check on a box with no keys reports every absent credential, writes nothing, and exits 1.
func TestCheckPipedReportsAbsentKeysAndExitsOne(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	vaulttest.Use(t) // pins are vault-only: seed the openai pin, then assert --check leaves the vault bytes alone
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	putRec(t, "claude", "PROFILE", "openai")
	vaultBefore, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db"))
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh/codex/copilot on PATH: nothing can look "present" by accident
	out, rc := runKeysCheck(t, Options{Check: true, Home: home, In: strings.NewReader("")})
	if rc != 1 {
		t.Errorf("rc = %d, want 1; output:\n%s", rc, out.String())
	}
	for _, want := range []string{"== keys ==", "ABSENT", "claude-code vault set OPENAI_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if after, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db")); !bytes.Equal(after, vaultBefore) {
		t.Error("--check wrote vault.db")
	}
}

// A vendor whose only resolvable provider is another vendor's passthrough login is ABSENT, not "present".
func TestVendorKeyTargetRejectsForeignPassthrough(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	out, _ := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if !strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "codex      present (subscription") {
		t.Errorf("codex must not read as present on a subscription session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "codex login") {
		t.Errorf("codex row should name `codex login` as the fix:\n%s", out.String())
	}
}

// A vendor running on its own stored config (opencode.jsonc sets `model`) is configured, not ABSENT: the runner
// launches it untouched, so `--check` must not demand a key for it.
// opencode's own opencode.jsonc "model" setting is no longer a fallback dispatch (DescribeRoute's
// copilot-login -> anthropic-key -> none chain replaced it): with no credential on
// that chain, the vendor reads ABSENT and the fix command names `claude-code configure github`.
func TestVendorOnOwnConfigReadsAbsentUnderNewChain(t *testing.T) {
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "opencode", "opencode.jsonc"), []byte(`{"model": "google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "opencode", Home: home, In: strings.NewReader("")})
	if rc != 1 || !strings.Contains(out.String(), "opencode") || !strings.Contains(out.String(), "ABSENT") {
		t.Errorf("rc = %d; opencode.jsonc's own model must no longer read present:\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "google/gemini-3.8-flash") {
		t.Errorf("the retired own-config dispatch must not appear in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "claude-code configure github") {
		t.Errorf("fix command must name `claude-code configure github`:\n%s", out.String())
	}
}

// An absent credential offers BOTH the browser login and a pasted key; piped, each is one [Y/n], so declining
// the login and accepting the key seals it in the vault without opening a browser.
func TestResolveKeyOffersLoginAndKeyThenStoresPastedKey(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	kt := configure.KeyTarget{Label: "openai", EnvVar: "OPENAI_API_KEY", LoginArgv: []string{"openai", "login"}, FixCmd: "x"}
	opts := configure.AuthOptions(kt)
	if len(opts) != 2 || opts[0].Argv == nil || opts[1].Env != "OPENAI_API_KEY" || !strings.Contains(opts[1].Desc, "platform.openai.com") {
		t.Fatalf("authOptions = %+v, want browser login then key with its console URL", opts)
	}
	in := strings.NewReader("n\ny\nn\n  sk-test-value \n")
	var out bytes.Buffer
	d := &dialog{o: Options{Interactive: true, In: in, Out: &out}, br: bufio.NewReader(in), out: &out}
	if !resolveKey(d, kt) {
		t.Fatalf("resolveKey = false\n%s", out.String())
	}
	got, src, ok := secretstore.Get("OPENAI_API_KEY")
	if !ok || src != "vault" || got != "sk-test-value" {
		t.Errorf("stored key = (%q, %q, %v)", got, src, ok)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if raw, _ := tx.Get(vault.Secrets, "OPENAI_API_KEY"); strings.Contains(raw, "sk-test-value") {
			t.Error("vault record contains the plaintext secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "logged in") {
		t.Error("login must not have run after being declined")
	}
	for _, want := range []string{"Browser login — openai login? [Y/n]", "Paste API key — create at platform.openai.com/api-keys"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dialog lacks %q:\n%s", want, out.String())
		}
	}
}

// --check on a present, rotatable credential (codex's ChatGPT login) must stay read-only: no "keep it?"
// prompt, no rotate CLI invoked, no file write — the new rotate feature must not change --check's contract.
func TestCheckStaysReadOnlyWithRotatableCredential(t *testing.T) {
	cfg, home, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check, never execs a CLI
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (codex reads present, nothing to fix):\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "current credential as-is") || strings.Contains(out.String(), "logged in again") {
		t.Errorf("--check must never prompt to rotate or run a rotate CLI:\n%s", out.String())
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "vault.db")); err == nil {
		t.Error("--check wrote vault.db")
	}
}

// `keys` was removed from Sections: naming it as a section now reports "unknown section" and exits 2,
// never falling through to the key report (that report still runs internally, via Vendor -> runKeysChanged).
func TestRunUnknownSectionKeysReturnsTwo(t *testing.T) {
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "keys")
	if rc != 2 {
		t.Errorf("rc = %d, want 2:\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "unknown section") {
		t.Errorf("output lacks %q:\n%s", "unknown section", out.String())
	}
}

// "claude" is a Sections entry: present in the walk order, and listed in the unknown-section error so
// a typo'd section name tells the user it is a valid choice.
func TestClaudeSectionIsRegistered(t *testing.T) {
	if !slices.Contains(SectionNames, "claude") {
		t.Fatalf("SectionNames = %v, want \"claude\" present", SectionNames)
	}
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "bogus-section")
	if rc != 2 {
		t.Errorf("rc = %d, want 2:\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "claude") {
		t.Errorf("unknown-section output lacks %q:\n%s", "claude", out.String())
	}
}

// The Max-plan row offers three ways in: browser login, minting a headless token, pasting one; the minted token
// is recognised in the CLI's output by its sk-ant-oat prefix.
func TestMaxPlanOptionsAndSetupTokenCapture(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: status reads as not logged in
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	kt := configure.MaxPlanTarget(configure.KeyTarget{Label: "claude"})
	if kt.Present || kt.Headless {
		t.Fatalf("no login and no token must read absent: %+v", kt)
	}
	opts := configure.AuthOptions(kt)
	if len(opts) != 3 || opts[0].Argv == nil || opts[1].Mint == nil || opts[1].Env != "CLAUDE_CODE_OAUTH_TOKEN" || opts[2].Env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("authOptions = %+v, want login, mint, paste", opts)
	}
	out := "Opening browser...\nPaste the code here:\n\nYour token: sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789\n"
	if got := configure.SetupTokenRE.FindString(out); got != "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789" {
		t.Errorf("token capture = %q", got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored")
	if kt := configure.MaxPlanTarget(configure.KeyTarget{Label: "claude"}); !kt.Present || !kt.Headless || !strings.Contains(kt.Source, "headless token") {
		t.Errorf("a stored token must read present/headless: %+v", kt)
	}
}

const configureTestToken = "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789"

// configureFakeClaude logs argv, then acts on CONFIGURE_COMMAND_MODE; `exec sleep` leaves SIGINT nothing to defer.
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
	orig := actionConfirmed
	actionConfirmed = func(string) bool { return true }
	t.Cleanup(func() { actionConfirmed = orig })
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
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
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

func TestLoginAndMintContext(t *testing.T) {
	for _, mint := range []bool{false, true} {
		action := "login"
		if mint {
			action = "mint"
		}
		for _, mode := range []string{"normal", "fail", "cancel"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				bin, log, ready := configureCommandEnv(t, mode)
				argv := []string{bin, action, "argument with spaces"}
				if !mint {
					argv[0] = "claude"
				}
				var token string
				run := func(ctx context.Context) error {
					if mint {
						var err error
						token, err = configure.RunMintContext(ctx, argv)
						return err
					}
					return configure.RunLoginContext(ctx, argv)
				}
				if mode == "cancel" {
					assertConfigureCancellation(t, ready, run)
					assertConfigureArgv(t, log, argv[1:])
					return
				}
				var err error
				out := captureConfigureStdout(t, func() { err = run(context.Background()) })
				if mode == "fail" {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
						t.Errorf("command error = %v, want exit status 7", err)
					}
					if token != "" {
						t.Errorf("failed mint returned token %q", token)
					}
				} else {
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
				assertConfigureArgv(t, log, argv[1:])
			})
		}
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
			got, err := configure.CliArgv(argv)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{bin, "login", "argument with spaces"}; !slices.Equal(got, want) {
				t.Errorf("vendorArgv = %v, want %v", got, want)
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
	got, err := configure.CliArgv(argv)
	if err != nil || !slices.Equal(got, argv) {
		t.Errorf("vendorArgv = %v, %v; want %v, nil", got, err, argv)
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
	got, err := configure.CliArgv(argv)
	if got != nil || err == nil || err.Error() != wantErr.Error() {
		t.Errorf("vendorArgv = %v, %v; want nil, %v", got, err, wantErr)
	}
	if !slices.Equal(argv, original) {
		t.Errorf("input changed to %v, want %v", argv, original)
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
	kt := configure.AgentKeyTarget(reg, "copilot")
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
	kt := configure.AgentKeyTarget(reg, "codex")
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
	kt := configure.AgentKeyTarget(reg, "gemini")
	if !kt.Present {
		t.Fatalf("gemini = %+v, want present via the agy login marker", kt)
	}
	if len(kt.LoginArgv) != 1 || kt.LoginArgv[0] != "agy" {
		t.Errorf("gemini loginArgv = %v, want [agy]", kt.LoginArgv)
	}
}

// runRealRoute drives Set up's real route step (picker + pin write) after a verified agent, answering the picker with in.
func runRealRoute(t *testing.T, in string, readOnlyVault bool) string {
	t.Helper()
	reg := menuTestEnv(t)
	putRec(t, "codex", "PROFILE", "chatgpt") // codex is on its own login, so picking off is a real write
	if readOnlyVault {
		if err := os.Chmod(vault.Path(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(vault.Path(), 0o600) })
	}
	st, ve := setupState, setupVerify
	t.Cleanup(func() { setupState, setupVerify = st, ve })
	setupState = func(*dialog, string) agentState { return agentState{verified: true} }
	setupVerify = func(context.Context, string) error { return nil }
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out, br: bufio.NewReader(strings.NewReader(in))}
	agentSetUp(d, "codex")
	return out.String()
}

// A pin write that fails must end Set up with "stopped at route", not read as a dismissed picker.
func TestSetUpRouteFailedPinWriteIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	got := runRealRoute(t, "1\n", true)
	if !strings.Contains(got, "set up stopped at route:") || strings.Contains(got, "the route was not changed") {
		t.Errorf("failed write: out = %q, want 'stopped at route' and no 'not changed'", got)
	}
}

// A dismissed picker stays quiet: only "the route was not changed", no error.
func TestSetUpRouteCancelIsQuiet(t *testing.T) {
	got := runRealRoute(t, "\n", false)
	if !strings.Contains(got, "signed in; the route was not changed") || strings.Contains(got, "stopped at") {
		t.Errorf("cancel: out = %q, want only 'route was not changed'", got)
	}
}

// maxRuneLen is the widest label (rune count) among rows a picker renders together.
func maxRuneLen(labels []string) int {
	w := 0
	for _, l := range labels {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	return w
}

// rowRenderedWidth mirrors tui's layoutRows + render: a 2-char marker/space prefix, every label padded to
// the widest in its row set, a 2-space spacer, then the detail column.
func rowRenderedWidth(labels []string, detail string) int {
	return 2 + maxRuneLen(labels) + 2 + len([]rune(ansi.Strip(detail)))
}

// TestMenuRowWidthsFitEightyColumns: every row configure shows — the top agent picker and each agent's own
// submenu — must render at 80 columns or fewer; past that it wraps on an ordinary terminal, which (per the
// tui package's own wrapped-line accounting) is exactly what desyncs a redraw's rewind count.
func TestMenuRowWidthsFitEightyColumns(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}

	rows, _ := menuRows(d)
	rows = append(rows, submitRow)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	for _, r := range rows {
		if w := rowRenderedWidth(labels, r[1]); w > 80 {
			t.Errorf("top menu row %q = %d cols, want <=80: %q", r[0], w, r[1])
		}
	}

	for name, build := range agentMenus {
		m := build(d)
		var subLabels []string
		for _, r := range m.rows {
			subLabels = append(subLabels, r.label)
		}
		subLabels = append(subLabels, "Back")
		for _, r := range m.rows {
			if w := rowRenderedWidth(subLabels, r.desc); w > 80 {
				t.Errorf("%s submenu row %q = %d cols, want <=80: %q", name, r.label, w, r.desc)
			}
		}
	}
}

// TestHomeExtraRowsFitEightyColumns renders the tools/settings rows with worst-case state text.
func TestHomeExtraRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	rows := []homeRow{
		{Name: "tailscale", State: "not connected", Check: "a-long-hostname-01 · no admin key", Run: nil},
		{Name: "web search", State: "no key", Check: "mcp: claude,codex", Run: nil},
		{Name: "browsers", State: "default chrome", Check: "12 installed", Run: nil},
	}
	rows = append(rows, settingsRows(&dialog{})...)
	laid, _ := RowsOf(rows)
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
}

// TestSettingRowFitsEightyColumns: the widest "claude code" page row — the longest catalog key, its
// longest enum value or "false", plus the worst-case source suffix " · live differs" — stays <=80 cols.
func TestSettingRowFitsEightyColumns(t *testing.T) {
	var labels []string
	for _, f := range settings.Catalog() {
		labels = append(labels, f.Key)
	}
	for _, f := range settings.Catalog() {
		longest := "false"
		for _, v := range f.Values {
			if len(v) > len(longest) {
				longest = v
			}
		}
		tmpl := map[string]json.RawMessage{f.Key: json.RawMessage(`"` + longest + `"`)}
		ov := map[string]json.RawMessage{}
		live := map[string]json.RawMessage{f.Key: json.RawMessage(`"` + longest + "-differs" + `"`)}
		row := settingRow(f, tmpl, ov, live)
		if w := rowRenderedWidth(labels, row[1]); w > 80 {
			t.Errorf("settingRow(%s) = %d cols, want <=80: %q", f.Key, w, row[1])
		}
	}
}

// TestRoutingRowFitsEightyColumns: the widest "routing" page row — the longest field key, its longest
// value, plus the "default"/"vault" source suffix — stays <=80 cols.
func TestRoutingRowFitsEightyColumns(t *testing.T) {
	var labels []string
	for _, m := range agentconfig.Modules() {
		for _, f := range m.Fields() {
			labels = append(labels, f.Key)
		}
	}
	worst := providers.Routing{
		Weights: map[string]float64{"price": 0.123456789, "headroom": 0.876543211},
		Opencode: providers.OpencodeRouting{
			Ladder:          &providers.RouteOpencodeSubIDs,
			WorkRepoMarkers: &[]string{"gitlab.example.com", "github.example.net"},
			FreeTiers:       &[]string{"sonnet", "haiku"},
			FreeFirstOutput: "90s",
		},
	}
	for _, m := range agentconfig.Modules() {
		for _, f := range m.Fields() {
			detail := f.Get(worst) + "  " + routingSource(f, worst)
			if w := rowRenderedWidth(labels, detail); w > 80 {
				t.Errorf("routing field %s = %d cols, want <=80: %q", f.Key, w, detail)
			}
		}
	}
}

// TestTierRowsFitEightyColumns: alias · use · a long model id stays within 80 columns.
func TestTierRowsFitEightyColumns(t *testing.T) {
	for alias, use := range aliasUse {
		row := fmt.Sprintf("  %-6s  %s", alias, PadTo(use, 42)+"anthropic/claude-sonnet-5-5") // as tui.layoutRows lays it
		if w := len([]rune(row)); w > 80 {
			t.Errorf("%s alias row is %d columns: %q", alias, w, row)
		}
	}
}

// TestDatabricksRowsFitEightyColumns: the home row at its worst state text and every page/env/apply row stay <=80 cols.
func TestDatabricksRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	rows := []homeRow{{Name: "databricks", State: "prod · oauth-m2m", Check: "expires in 119m", Run: nil}, {Name: "databricks", State: "prod · oauth-m2m", Check: "expires in 23h59m", Run: nil}}
	rows = append(rows, settingsRows(&dialog{})...)
	laid, _ := RowsOf(rows)
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
	d := dbxEnv(t)
	if row := databricksRow(d); 2+len([]rune("environment"))+2+len([]rune(row.State))+2+len([]rune(row.Check)) > 80 {
		t.Errorf("live databricks row too wide: %+v", row)
	}
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{1, 4, 0, -1, -1}
	runDatabricksPage(d)
	for i, set := range f.rows {
		var labels []string
		for _, r := range set {
			labels = append(labels, r[0])
		}
		for _, r := range set {
			if w := rowRenderedWidth(labels, r[1]); w > 80 {
				t.Errorf("%s row %q = %d cols: %q", f.titles[i], r[0], w, r[1])
			}
		}
	}
	worst := "prod"
	if w := rowRenderedWidth([]string{"dev", "qa", worst}, "oauth-m2m  token expires in 23h59m"); w > 80 {
		t.Errorf("env row worst case = %d cols", w)
	}
}

func TestSecretLineEchoesMaskedPreview(t *testing.T) {
	for _, in := range []string{"tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n",
		"TAILSCALE_OAUTH_CLIENT_SECRET=tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n"} {
		var out strings.Builder
		d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader(in))}
		if _, err := secretLine(d, "> "); err != nil {
			t.Fatal(err)
		}
		if w := len([]rune(strings.Split(strings.TrimSpace(out.String()), "\n")[1])); w > 80 {
			t.Errorf("preview line is %d columns", w)
		}
		if strings.Contains(out.String(), "0123456789") || !strings.Contains(out.String(), "(49 chars)") {
			t.Errorf("preview leaks or lacks length: %q", out.String())
		}
	}
}

func TestSecretLineFallbackPreservesInputAndOutput(t *testing.T) {
	for _, in := range []string{"", "\n", "  plain value  \r\n", "NAME=private-value\n", "unterminated-value"} {
		t.Run(strings.TrimSpace(in), func(t *testing.T) {
			d, out := newDialog(in)
			got, err := secretLine(d, "> ")
			want := strings.TrimRight(in, "\r\n")
			if err != nil || got != want {
				t.Fatalf("secretLine = (%q, %v), want (%q, nil)", got, err, want)
			}
			wantOut := "> \n"
			if _, value := configure.ParseAssignment(want); value != "" {
				wantOut += "  pasted  " + pastePreview(want) + "\n"
			}
			if out.String() != wantOut {
				t.Errorf("output = %q, want %q", out.String(), wantOut)
			}
		})
	}
}

// TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys: y stores the page's staged keys (Submit has none left),
// N restores env; a key staged before the page opened is untouched either way.
func TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys(t *testing.T) {
	for _, yes := range []bool{true, false} {
		menuTestEnv(t)
		t.Setenv("OLD_KEY", "")
		os.Unsetenv("PAGE_KEY")
		d := &dialog{out: discardWriter{}, pending: map[string]stagedKey{}}
		d.stage("OLD_KEY", "old")
		before := map[string]stagedKey{"OLD_KEY": d.pending["OLD_KEY"]}
		d.stage("PAGE_KEY", "page-value")
		orig := saveConfirmed
		saveConfirmed = func(string) bool { return yes }
		confirmPageKeys(d, before)
		saveConfirmed = orig
		if _, still := d.pending["PAGE_KEY"]; still {
			t.Errorf("yes=%v: PAGE_KEY still pending after the page closed", yes)
		}
		if _, ok := d.pending["OLD_KEY"]; !ok {
			t.Errorf("yes=%v: OLD_KEY (staged before the page) must stay for Submit", yes)
		}
		if got := os.Getenv("PAGE_KEY"); (got == "page-value") != yes {
			t.Errorf("yes=%v: PAGE_KEY env = %q", yes, got)
		}
		d.pending = nil
		os.Unsetenv("PAGE_KEY")
	}
}
