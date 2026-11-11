package databricks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// call is one recorded spawn of the child.
type call struct {
	bin  string
	argv []string
	env  []string
}

// fakeChild records every spawn and answers the smoke prompt from its script.
type fakeChild struct {
	calls []call
	smoke string // stdout for the smoke prompt
	code  int
	err   error
}

func (f *fakeChild) spawn(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, call{bin: bin, argv: slices.Clone(argv), env: slices.Clone(env)})
	if f.err != nil {
		return 0, f.err
	}
	_, _ = io.WriteString(stdout, f.smoke)
	return f.code, nil
}

// useFake swaps the spawn seam for f and restores the real one at cleanup.
func useFake(t *testing.T, f *fakeChild) {
	t.Helper()
	old := spawn
	spawn = f.spawn
	oldCtx := spawnCtx
	spawnCtx = func(_ context.Context, bin string, argv, env []string, in io.Reader, out, errw io.Writer) (int, error) {
		return f.spawn(bin, argv, env, in, out, errw)
	}
	t.Cleanup(func() { spawn, spawnCtx = old, oldCtx })
}

// argValue returns the element after flag in argv, or "".
func argValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// threeProfiles defines three opaque Databricks profiles so the plan tests never read the real vault.
const threeProfiles = `{"providers":{"databricks":{"aa":{"preset":"databricks",` + dbxHost + `},"bb":{"preset":"databricks",` +
	dbxHost + `},"cc":{"preset":"databricks",` + dbxHost + `}}}}`

// opusDefault is the default model for the seeded rows: the opus alias at 1M, as the row spells it.
func opusDefault(t *testing.T, name string) string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil || reg.Providers[name] == nil {
		t.Fatalf("profile %q: %v", name, err)
	}
	return reg.Providers[name].WireModel(aliasID(models.Opus))
}

// TestPlanTable pins the default model, profile selection, passthrough order and --profile forms.
func TestPlanTable(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	def := opusDefault(t, "bb")
	tests := []struct {
		name     string
		args     []string
		wantProv string
		wantMod  string
		wantRest []string
	}{
		{"model only", []string{"sonnet"}, "", "sonnet", nil},
		{"--profile bb", []string{"--profile", "bb"}, "bb", def, nil},
		{"--profile after flags", []string{"--print", "hi", "--profile", "cc"}, "cc", def, []string{"--print", "hi"}},
		{"--profile= form", []string{"--profile=bb"}, "bb", def, nil},
		{"model + --profile=cc", []string{"haiku", "--profile=cc"}, "cc", "haiku", nil},
		{"rest order kept", []string{"--profile", "cc", "ask", "hello world"}, "cc", def, []string{"ask", "hello world"}},
		{"flags-first keeps default model", []string{"--print", "hi", "--profile=bb"}, "bb", def, []string{"--print", "hi"}},
		{"only MODEL is taken", []string{"sonnet", "haiku", "--profile=bb"}, "bb", "sonnet", []string{"haiku"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			want := tt.wantProv
			if want == "" {
				want = providers.DatabricksProfiles(mustLoad(t))[0]
			}
			if p.Provider != want || p.Model != tt.wantMod && tt.wantProv != "" || p.Model == "" {
				t.Errorf("got profile %q model %q, want %q %q", p.Provider, p.Model, want, tt.wantMod)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

func mustLoad(t *testing.T) *providers.Registry {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestResolveProfileOrder pins explicit, then session, then the CLAUDE_PROFILE pin, then the first Databricks profile.
func TestResolveProfileOrder(t *testing.T) {
	seedOverlay(t, threeProfiles)
	reg := mustLoad(t)
	first := providers.DatabricksProfiles(reg)[0]
	t.Setenv(providers.EnvSessionProfile, "")
	if got := resolveProfile(reg, "zz"); got != "zz" {
		t.Errorf("explicit = %q, want zz", got)
	}
	if got := resolveProfile(reg, ""); got != first {
		t.Errorf("no pin = %q, want the first profile %q", got, first)
	}
	vaulttest.SetRecord(t, "claude", "profile", "cc")
	if got := resolveProfile(reg, ""); got != "cc" {
		t.Errorf("pinned = %q, want cc", got)
	}
	t.Setenv(providers.EnvSessionProfile, "bb")
	if got := resolveProfile(reg, ""); got != "bb" {
		t.Errorf("session = %q, want bb", got)
	}
	t.Setenv(providers.EnvSessionProfile, "not-databricks") // shadows the pin of the same name
	if got := resolveProfile(reg, ""); got != first {
		t.Errorf("non-Databricks session = %q, want the first profile %q", got, first)
	}
}

// TestPlanCheck pins the leading check token: only the first arg turns check on.
func TestPlanCheck(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "bb")
	tests := []struct {
		name      string
		args      []string
		wantCheck bool
		wantProv  string
		wantRest  []string
	}{
		{"check alone", []string{"check"}, true, "bb", nil},
		{"check model profile", []string{"check", "haiku", "--profile", "cc"}, true, "cc", nil},
		{"check not first is a rest arg", []string{"sonnet", "check"}, false, "bb", []string{"check"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			if p.Check != tt.wantCheck || p.Provider != tt.wantProv {
				t.Errorf("check=%v profile=%q, want %v %q", p.Check, p.Provider, tt.wantCheck, tt.wantProv)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

// TestPlanProfileErrors pins that a name outside the Databricks profiles is refused, including no value.
func TestPlanProfileErrors(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	tests := map[string][]string{
		"unknown":              {"--profile", "staging"},
		"= unknown":            {"--profile=staging"},
		"no value":             {"--profile"},
		"= (empty)":            {"--profile="},
		"eats a flag":          {"--profile", "--print"},
		"after model, unknown": {"sonnet", "--profile", "stage"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := plan(args)
			if name == "no value" || name == "= (empty)" {
				if err != nil { // an empty name falls back to the workspace's last profile, then the pin
					t.Fatalf("plan(%q) error = %v", args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "not a Databricks profile") {
				t.Fatalf("plan(%q) = %+v, %v; want a profile error", args, p, err)
			}
		})
	}
}

// TestRunHelpOnlyFirstOrAfterCheck pins that -h is help only as the first arg or right after check.
func TestRunHelpOnlyFirstOrAfterCheck(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, args := range [][]string{{"-h"}, {"--help"}, {"check", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeChild{}
			useFake(t, f)
			code, _, _ := hosttest.CaptureOutput(t, func() int { return Run(args) })
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if len(f.calls) != 0 {
				t.Errorf("help launched the child %d time(s)", len(f.calls))
			}
		})
	}
}

// TestRunLaterHelpPassesThrough pins that -h after a model reaches claude, as the shell wrapper does.
func TestRunLaterHelpPassesThrough(t *testing.T) {
	seedOverlay(t, threeProfiles)
	tests := [][]string{{"sonnet", "--print", "-h"}, {"--profile", "bb", "-h"}}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeChild{}
			useFake(t, f)
			if code := Run(args); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if len(f.calls) != 1 || !slices.Contains(f.calls[0].argv, "-h") {
				t.Fatalf("calls = %+v, want one launch carrying -h", f.calls)
			}
		})
	}
}

// TestRunProfileErrorExit2 pins exit 2 and no child start for an unknown profile.
func TestRunProfileErrorExit2(t *testing.T) {
	seedOverlay(t, threeProfiles)
	f := &fakeChild{}
	useFake(t, f)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"--profile", "staging"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not a Databricks profile") {
		t.Errorf("stderr = %q, want profile error", stderr)
	}
	if len(f.calls) != 0 {
		t.Errorf("bad profile launched the child")
	}
}

// TestLaunchArgvNoShellAndBetaEnv pins the list-argv launch; the beta opt-out is covered below.
func TestLaunchArgvNoShellAndBetaEnv(t *testing.T) {
	seedOverlay(t, threeProfiles)
	f := &fakeChild{}
	useFake(t, f)
	if code := Run([]string{"sonnet", "--profile", "cc", "--print", "hi"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.calls) != 1 {
		t.Fatalf("spawns = %d, want 1", len(f.calls))
	}
	c := f.calls[0]
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if c.bin != self {
		t.Errorf("bin = %q, want this binary %q", c.bin, self)
	}
	want := []string{"--profile", "cc", "--model", "sonnet", "--print", "hi"}
	if !slices.Equal(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
	for _, shell := range []string{"sh", "bash", "-c"} {
		if slices.Contains(c.argv, shell) || strings.HasSuffix(c.bin, shell) {
			t.Errorf("launch looks shell-wrapped via %q", shell)
		}
	}
}

// TestLaunchPropagatesExitAndStartError pins the child's exit code and exit 2 on a start failure.
func TestLaunchPropagatesExitAndStartError(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Run("child exit 7", func(t *testing.T) {
		useFake(t, &fakeChild{code: 7})
		if code := Run([]string{"sonnet"}); code != 7 {
			t.Errorf("exit = %d, want 7", code)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		useFake(t, &fakeChild{err: os.ErrNotExist})
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"sonnet"}) })
		if code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(stderr, "exec failed") {
			t.Errorf("stderr = %q, want exec failed", stderr)
		}
	})
}

// TestCheckSmokeVerdict pins that the smoke PASS needs the last line to be exactly OK.
func TestCheckSmokeVerdict(t *testing.T) {
	seedOverlay(t, threeProfiles)
	tests := []struct {
		name   string
		out    string
		wantOK bool
	}{
		{"bare OK", "OK\n", true},
		{"OK no newline", "OK", true},
		{"OK after noise", "thinking...\nOK\n", true},
		{"OK then more text", "OK\nmore\n", false},
		{"substring only", "NOT OK\n", false},
		{"empty output", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFake(t, &fakeChild{smoke: tt.out})
			code, stdout, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"check"}) })
			if tt.wantOK {
				if code != 0 || !strings.Contains(stdout, "PASS smoke") {
					t.Errorf("exit %d, stdout %q; want PASS smoke and exit 0", code, stdout)
				}
				return
			}
			if code != 1 || !strings.Contains(stdout, "FAIL smoke") {
				t.Errorf("exit %d, stdout %q; want FAIL smoke and exit 1", code, stdout)
			}
		})
	}
}

// A removed flag exits 2 with its hint before any subverb, probe or child launch.
func TestRunRemovedFlagsExit2NoLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"--env", "qa"}, {"check", "--env", "qa"}, {"check", "--provider=x"}, {"--tier", "opus"}, {"--router"}, {"check", "--1m"},
	} {
		f := &fakeChild{smoke: "OK\n"}
		useFake(t, f)
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
		if code != 2 || !strings.Contains(stderr, "--") || strings.Contains(stdout, "PASS") {
			t.Errorf("Run(%q) = %d, stdout %q, stderr %q; want exit 2 with a hint", args, code, stdout, stderr)
		}
		if len(f.calls) != 0 {
			t.Errorf("Run(%q) launched the child: %v", args, f.calls)
		}
	}
}

func TestCheckHelpShowsCheckPageNotTopLevel(t *testing.T) {
	code, out, errs := captureOutput(t, func() int { return Run([]string{"check", "--help"}) })
	all := out + errs
	if code != 0 || !strings.Contains(all, "databricks check [MODEL]") {
		t.Fatalf("check --help: exit %d, want check's own page:\n%s", code, all)
	}
	// Negative control: the plain page is the launcher's.
	_, out, errs = captureOutput(t, func() int { return Run([]string{"--help"}) })
	if strings.Contains(out+errs, "databricks check [MODEL]") {
		t.Errorf("top-level help shows check's usage:\n%s", out+errs)
	}
}

// -p NAME selects the same profile as --profile NAME through bind() (config, rescue).
func TestProfileShorthandMatchesLongFlag(t *testing.T) {
	for _, args := range [][]string{{"-p", "bb"}, {"-p=bb"}, {"--profile", "bb"}} {
		var f cfgFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		bind(fs, &f, "harness", true)
		if err := fs.Parse(args); err != nil || f.profile != "bb" {
			t.Errorf("bind %v: profile=%q err=%v, want bb", args, f.profile, err)
		}
	}
}

// -p names the profile on check and on the bare launch verb; claude's print mode passes through as --print.
func TestPlanShorthandProfile(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	c, err := plan([]string{"check", "-p", "cc"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := plan([]string{"-p", "aa", "--print", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "cc" || len(c.Rest) != 0 {
		t.Errorf("check -p cc: provider %q rest %v", c.Provider, c.Rest)
	}
	if l.Provider != "aa" || len(l.Rest) != 2 || l.Rest[0] != "--print" {
		t.Errorf("launch -p aa: provider %q rest %v", l.Provider, l.Rest)
	}
}

const dbxHost = `"host":"https://adb-1.azuredatabricks.net"`

// seedOverlay isolates the vault in a temp config dir and writes doc as the providers overlay ("" = none).
func seedOverlay(t *testing.T, doc string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if doc == "" {
		return
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

// presetOnly defines one preset profile (which injects the beta opt-out into its Env).
const presetOnly = `{"providers":{"databricks":{"aa":{"preset":"databricks",` + dbxHost + `}}}}`

func TestPlanNoBetaOnlyWhenProviderEnvHasKey(t *testing.T) {
	seedOverlay(t, presetOnly)
	p, err := plan([]string{"sonnet", "--profile", "aa"})
	if err != nil || !p.NoBeta {
		t.Errorf("preset provider Env carries the beta key: NoBeta should be true (err %v)", err)
	}
	if providerHasBetaOff(nil, "aa") || providerHasBetaOff(mustLoad(t), "missing") {
		t.Error("unknown profile: NoBeta should be false")
	}
}

func TestChildEnvBeta(t *testing.T) {
	t.Setenv(betaKey, "")
	os.Unsetenv(betaKey)
	if slices.Contains(childEnv(launchPlan{NoBeta: true}), betaOff) {
		t.Error("NoBeta plan must not inject the opt-out")
	}
	if !slices.Contains(childEnv(launchPlan{}), betaOff) {
		t.Error("plan without NoBeta must inject the opt-out")
	}
}

func TestLaunchBetaInjectionFollowsProvider(t *testing.T) {
	t.Setenv(betaKey, "")
	os.Unsetenv(betaKey)
	seedOverlay(t, presetOnly)
	f := &fakeChild{}
	useFake(t, f)
	if code := Run([]string{"sonnet", "--profile", "aa"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if slices.Contains(f.calls[0].env, betaOff) {
		t.Error("preset provider owns the opt-out: launch must not inject it")
	}
}

// useVaultTest isolates the vault in a temp config dir with a fresh master key.
func useVaultTest(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

// captureOutput runs fn with stdout/stderr captured.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	return hosttest.CaptureOutput(t, fn)
}

// TestPlanFailsClosedOnProviderLoadError pins that an unreadable registry is an error, not a skipped profile check.
func TestPlanFailsClosedOnProviderLoadError(t *testing.T) {
	seedOverlay(t, "{not json")
	t.Setenv(providers.EnvSessionProfile, "")
	if p, err := plan([]string{"--profile", "anything"}); err == nil || !strings.Contains(err.Error(), "load providers") {
		t.Fatalf("plan = %+v, %v; want a load providers error", p, err)
	}
}

func TestRunPreservesChildProfileAfterSeparator(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "bb")
	for _, flag := range []string{"--profile", "-p", "--profile=other", "-p=other"} {
		t.Run(flag, func(t *testing.T) {
			rest := []string{"--", flag}
			if !strings.Contains(flag, "=") {
				rest = append(rest, "other")
			}
			f := &fakeChild{}
			useFake(t, f)
			if code := Run(rest); code != 0 {
				t.Fatalf("exit = %d, want child launch", code)
			}
			want := append([]string{"--profile", "bb"}, rest...)
			if len(f.calls) != 1 || !slices.Equal(f.calls[0].argv, want) {
				t.Fatalf("calls = %+v, want argv %q", f.calls, want)
			}
		})
	}
}

func TestCheckForwardsModelFlag(t *testing.T) {
	seedOverlay(t, threeProfiles)
	f := &fakeChild{smoke: "OK\n"}
	useFake(t, f)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"check", "--profile", "bb", "--model", "sonnet"})
	})
	if code != 0 || !strings.Contains(out, "PASS smoke") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, stderr)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %+v, want one smoke call", f.calls)
	}
	argv := f.calls[0].argv
	if !slices.Equal(argv[len(argv)-2:], []string{"--model", "sonnet"}) {
		t.Errorf("smoke argv = %q, want forwarded --model sonnet", argv)
	}
}

func TestCutFlagFindsFlagAnywhere(t *testing.T) {
	rest, ok := cutFlag([]string{"--profile", "x", "--all", "--model", "m"}, "--all")
	if !ok || !slices.Equal(rest, []string{"--profile", "x", "--model", "m"}) {
		t.Errorf("cutFlag = %v, %v", rest, ok)
	}
	if _, ok := cutFlag([]string{"opus"}, "--all"); ok {
		t.Error("reported --all when absent")
	}
}

func TestSubverbCheckAllAfterOtherArgs(t *testing.T) {
	if _, ok := subverbExit([]string{"check", "--profile", "nope", "--all"}); !ok {
		t.Error("check ... --all not routed to the sweep")
	}
}

// A workspace's last claude profile beats the global vault pin when no session profile is set.
func TestResolveProfileUsesWorkspaceLastProfile(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.SetRecord(t, "claude", "profile", "cc")
	providers.RecordLastProfile("claude", "bb")
	if got := resolveProfile(mustLoad(t), ""); got != "bb" {
		t.Errorf("resolveProfile = %q, want the workspace's last profile bb", got)
	}
}

// A non-Databricks last-used profile must not shadow the pinned Databricks record profile.
func TestResolveProfileLastNonDatabricksFallsToPin(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.SetRecord(t, "claude", "profile", "cc")
	providers.RecordLastProfile("claude", "not-databricks")
	if got := resolveProfile(mustLoad(t), ""); got != "cc" {
		t.Errorf("resolveProfile = %q, want the pinned cc", got)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func TestCheckAllAcceptsPositionalModel(t *testing.T) {
	seedOverlay(t, "")
	t.Setenv("DATABRICKS_TEST_PROBE_TOKEN", "test-token")
	seen := make(chan string, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen <- body.Model
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	old := probeClient
	probeClient = srv.Client()
	t.Cleanup(func() { probeClient = old })
	if err := providers.SetProviderFields("test-probe", map[string]any{
		"preset": "databricks", "host": srv.URL,
		"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_PROBE_TOKEN"},
	}); err != nil {
		t.Fatal(err)
	}
	model := "claude-opus-5-5"
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"check", model, "--all", "--wire", "all", "--profile", "test-probe", "--model", "claude-haiku-4-5"})
	})
	if code != 0 || len(seen) != 2 {
		t.Fatalf("exit = %d, probes = %d, stdout = %q, stderr = %q", code, len(seen), out, stderr)
	}
	got := []string{<-seen, <-seen} // the wires probe concurrently
	slices.Sort(got)
	if want := []string{"databricks-" + model, "system.ai." + model}; !slices.Equal(got, want) {
		t.Errorf("probe models = %q, want positional model %q", got, want)
	}
}

func TestProbeReportsCutOffBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(status)
				if _, err := w.Write([]byte("partial")); err != nil {
					t.Errorf("write partial response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			p := &providers.Provider{Name: "p", Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL}
			want := "HTTP 200 (body cut off)"
			if status != http.StatusOK { // a non-2xx status stands on a cut body, labelled transient when it is one
				want = "transient HTTP 502: partial"
			}
			if got := probe(context.Background(), p, "test-token", "m"); got != want {
				t.Errorf("probe = %q, want %q", got, want)
			}
		})
	}
}

// probeServer is a TLS probe endpoint that holds each reply for delay and counts requests.
func probeServer(t *testing.T, delay time.Duration) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	old := probeClient
	probeClient = srv.Client()
	t.Cleanup(func() { probeClient = old })
	t.Setenv("DATABRICKS_TEST_PROBE_TOKEN", "test-token")
	for _, name := range []string{"p1", "p2", "p3"} {
		if err := providers.SetProviderFields(name, map[string]any{"preset": "databricks", "host": srv.URL,
			"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_PROBE_TOKEN"}}); err != nil {
			t.Fatal(err)
		}
	}
	return &n
}

// Profiles and their wires probe concurrently, and rows keep the requested order.
func TestCheckProfilesConcurrentInOrder(t *testing.T) {
	seedOverlay(t, "")
	const delay = 300 * time.Millisecond
	n := probeServer(t, delay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"p3", "p1", "p2"}
	start := time.Now()
	rows := checkProfiles(reg, names, "claude-haiku-5-5", []string{providers.WireServing, providers.WireAIGateway})
	if took := time.Since(start); took >= 2*delay || n.Load() != 6 {
		t.Fatalf("3 profiles x 2 wires took %s with %d probes; serial would be %s", took, n.Load(), 6*delay)
	}
	for i, r := range rows {
		if len(r.cells) != 2 || r.cells[0] != "ok" || r.cells[1] != "ok" {
			t.Errorf("row %d (%s) = %+v", i, names[i], r)
		}
	}
}

// Without --wire only the serving wire is probed and the header names only it; a bad --wire is refused.
func TestCheckAllDefaultsToServing(t *testing.T) {
	seedOverlay(t, "")
	n := probeServer(t, 0)
	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"check", "--all", "--profile", "p1"}) })
	if code != 0 || n.Load() != 1 || !strings.Contains(out, "SERVING") || strings.Contains(out, "AI-GATEWAY") {
		t.Fatalf("exit = %d, probes = %d, stdout = %q", code, n.Load(), out)
	}
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"check", "--all", "--wire", "bogus"}) }); code == 0 {
		t.Fatal("--wire bogus accepted")
	}
}

// A check's token request reports a 429 after one attempt; it never waits out the dispatch retries.
func TestCheckProfileTokenNoRetry(t *testing.T) {
	seedOverlay(t, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	oldBackoff := oauth.RetryBackoff
	oauth.RetryBackoff = time.Millisecond
	t.Cleanup(func() { oauth.RetryBackoff = oldBackoff })
	t.Setenv("DATABRICKS_NORETRY_M2M", "client-secret")
	row := &providers.Provider{Name: "noretry", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: srv.URL + "/serving-endpoints/anthropic", Auth: providers.Auth{Type: providers.AuthM2M, ClientID: "c"}}
	r := checkProfile(row, "noretry", "m", []string{providers.WireServing})
	if r.cells[0] != "no token" || calls.Load() != 1 {
		t.Fatalf("cells = %q, token calls = %d, want no token after 1 call", r.cells, calls.Load())
	}
	if _, err := oauth.MintM2M(context.Background(), srv.URL, "c", "client-secret"); err == nil || calls.Load() != 4 {
		t.Fatalf("a dispatch mint must keep its retries: calls = %d", calls.Load())
	}
}
