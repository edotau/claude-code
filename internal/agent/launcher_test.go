package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// headless is the only launch that carries the setup-token: -p and --print, in any position.
func TestHeadless(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-p", "hi"}, true},
		{[]string{"hi", "--print"}, true},
		{nil, false},
		{[]string{"-c"}, false},
		{[]string{"--settings", "x"}, false},
	} {
		if got := IsHeadless(tc.args); got != tc.want {
			t.Errorf("IsHeadless(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o, rest, err := ParseArgs([]string{"--profile=openai", "-p", "hi", "--model", "gpt-5", "--print-env", "--", "--model", "x"}, true)
	if err != nil || o.Provider != "openai" || o.Model != "gpt-5" || !o.PrintEnv {
		t.Fatalf("%+v %v", o, err)
	}
	if !slices.Equal(rest, []string{"-p", "hi", "--model", "x"}) {
		t.Errorf("rest %q", rest)
	}
	if _, _, err := ParseArgs([]string{"--profile"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// A prefixed model, typed or in --overlay, is a usage error (exit 2, no native fallback) that names the overlay once.
func TestParseArgsPrefixedModelIsUsage(t *testing.T) {
	for _, args := range [][]string{{"--model", "dev:opus"}, {"--overlay", `{"model":"dev:opus"}`}} {
		_, _, err := ParseArgs(args, true)
		if _, ok := errors.AsType[*UsageError](err); !ok || !errors.Is(err, providers.ErrModelSpec) ||
			strings.Count(err.Error(), "dev:opus") != 1 || !strings.Contains(err.Error(), "--model opus") {
			t.Errorf("%q: err = %v, want one usage refusal naming the overlay", args, err)
		}
	}
}

// --keep-env takes a comma list, repeatable, and accepts both the "--name value" and "--name=value" shapes.
func TestParseArgsKeepEnv(t *testing.T) {
	o, _, err := ParseArgs([]string{"--keep-env", "LANG,LC_ALL", "--keep-env=TZ"}, true)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"LANG", "LC_ALL", "TZ"}) {
		t.Fatalf("KeepEnv = %q, err %v", o.KeepEnv, err)
	}
	if _, _, err := ParseArgs([]string{"--keep-env"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// A credential name in --keep-env is refused whether or not a "--" follows; args after "--" pass through untouched.
func TestParseArgsKeepEnvCredentialRefusedWithAndWithoutDoubleDash(t *testing.T) {
	for _, args := range [][]string{
		{"--keep-env", "ANTHROPIC_API_KEY", "exec", "foo"},
		{"--keep-env", "ANTHROPIC_API_KEY", "--", "exec", "foo"},
	} {
		if _, _, err := ParseArgs(args, false); err == nil || !strings.Contains(err.Error(), "a credential name cannot be kept") {
			t.Errorf("ParseArgs(%v) err = %v, want the credential refusal", args, err)
		}
	}
	o, rest, err := ParseArgs([]string{"--keep-env", "FOO_HOME", "--", "exec", "--keep-env", "ANTHROPIC_API_KEY"}, false)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"FOO_HOME"}) || !slices.Equal(rest, []string{"exec", "--keep-env", "ANTHROPIC_API_KEY"}) {
		t.Errorf("benign name: keep=%v rest=%v err=%v; child args after -- must pass through unchanged", o.KeepEnv, rest, err)
	}
}

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := ParseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ParseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := ParseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}

// A launch refusal (*agent.UsageError, even wrapped) prints its message and exits 2, not the generic 1.
func TestRunPlanUsageErrorExitsTwo(t *testing.T) {
	err := fmt.Errorf("launch: %w", &UsageError{Msg: "not a harness profile"})
	if code := RunPlan("claude", Plan{}, LaunchOptions{}, err); code != 2 {
		t.Fatalf("RunPlan(UsageError) = %d, want 2", code)
	}
}
func TestInteractiveVendorArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"--version"}, false},
		{[]string{"-v"}, false},
		{[]string{"-V"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
		{[]string{"run", "x"}, false},
		{[]string{"--model", "m"}, true},
		{[]string{"chat", "--version"}, false},
	}
	for _, tc := range cases {
		if got := interactiveArgs(tc.args); got != tc.want {
			t.Errorf("interactiveVendorArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestLaunchProfilePrecedence(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "copilot", "profile", "ultra")
	vaulttest.SetRecord(t, "copilot", "model", "kimi-k3")
	if got := testLaunchProfile(t, "copilot", LaunchOptions{}); got.Provider != "ultra" || got.Model != "" {
		t.Errorf("pins only = %+v, want ultra and no model (the model pin applies after the target resolves)", got)
	}
	if got := testLaunchProfile(t, "copilot", LaunchOptions{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "copilot")
	if got := testLaunchProfile(t, "copilot", LaunchOptions{}); got.Provider != "copilot" {
		t.Errorf("own login = %q, want copilot", got.Provider)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if got := testLaunchProfile(t, "copilot", LaunchOptions{}); got.Provider != "" || got.Model != "" {
		t.Errorf("deactivated (no profile set) = %+v, want the pins ignored", got)
	}
}

const pinDbxRow = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"pat"}}}}}`

func TestAgentModelTargetPin(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	const m = "databricks-gpt-6-1-sol"
	vaulttest.SetRecord(t, "codex", "profile", "dev")
	vaulttest.SetRecord(t, "codex", "model", m)
	if got := testLaunchProfile(t, "codex", LaunchOptions{}); got.Provider != "dev" || got.Model != "" {
		t.Fatalf("codex = %+v, want the profile only", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := providers.Target{Provider: reg.Providers["dev"], Model: "base"}
	if got := PinnedTarget(t.Context(), "codex", dev); got.Model != m {
		t.Errorf("dev = %q, want %s kept", got.Model, m)
	}
	off := providers.Target{Provider: &providers.Provider{Name: "chatgpt"}, Model: "base"}
	if got := PinnedTarget(t.Context(), "codex", off); got.Model != "base" {
		t.Errorf("chatgpt = %q, want the databricks id skipped", got.Model)
	}
}

func TestPlainPlanRefusesWorkspaceModel(t *testing.T) {
	vaulttest.Use(t)
	pathstest.FakeVendor(t, "codex", "exit 0")
	spec := SchemaFor("codex")
	for _, args := range [][]string{
		{"--model", "databricks-gpt-6-1-sol"},
		{"--model=databricks-gpt-6-1-sol"},
	} {
		_, err := plainPlan(spec, args)
		if usage, ok := err.(*UsageError); !ok || !strings.Contains(usage.Msg, "refused on own login") {
			t.Fatalf("plainPlan(%q) = %v, want the own-login model refusal", args, err)
		}
	}
	if _, err := plainPlan(spec, []string{"--model", "gpt-6.1-sol"}); err != nil {
		t.Fatalf("bare model refused: %v", err)
	}
}

type launchModelSpy struct {
	fixtureDriver
	model *string
}

func (s launchModelSpy) RenderSettings(_ context.Context, st Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	*s.model = st.Target.Model
	return env, append(argv, "--reasoning-effort", "low"), []string{"effort medium→low"}, func() {}, nil
}

func TestPlainPlanPassesResolvedCopilotModel(t *testing.T) {
	vaulttest.Use(t)
	pathstest.FakeVendor(t, "copilot", "")
	var notice strings.Builder
	seen := ""
	registryMu.Lock()
	orig := drivers["copilot"]
	drivers["copilot"] = launchModelSpy{fixtureDriver{NewBaseEngine("copilot")}, &seen}
	registryMu.Unlock()
	out := NoticeOut
	NoticeOut = &notice
	t.Cleanup(func() {
		registryMu.Lock()
		drivers["copilot"] = orig
		registryMu.Unlock()
		NoticeOut = out
	})
	args := []string{"--model", "kimi-k3", "--resume"}
	plan, err := plainPlan(SchemaFor("copilot"), args)
	want := append([]string{"--reasoning-effort", "low"}, args...)
	if err != nil || seen != "kimi-k3" || !slices.Equal(plan.Argv, want) {
		t.Fatalf("model = %q, argv = %q, err = %v", seen, plan.Argv, err)
	}
	if notice.String() != "effort medium→low\n" {
		t.Fatalf("notice = %q", notice.String())
	}
}

func TestPlainPlansResolveVendorBinNotPATH(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			setConfigDir(t)
			source := pathstest.FakeVendorOutsideConfig(t, agentid.Binary(name), "")
			t.Setenv("PATH", filepath.Dir(source))
			bin := pathstest.FakeVendor(t, name, "")
			spec, ok := LookupSchema(name)
			if !ok {
				t.Fatalf("missing vendor %s", name)
			}
			plan, err := plainPlan(spec, nil)
			if err != nil || plan.Binary != bin {
				t.Fatalf("plainPlan = %q, %v; want %q", plan.Binary, err, bin)
			}
			if err := os.Remove(bin); err != nil {
				t.Fatal(err)
			}
			_, err = plainPlan(spec, nil)
			if err == nil || !strings.Contains(err.Error(), "not installed at "+bin) {
				t.Fatalf("missing vendor: %v; want bin-dir error despite PATH vendor", err)
			}
		})
	}
}

// D1: a launch with no --profile takes the agent pin; there is no last-used profile.
func TestLaunchProfileIgnoresLastUsed(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "antigravity")
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	if got := testLaunchProfile(t, "opencode", LaunchOptions{}).Provider; got != "antigravity" {
		t.Errorf("opencode: got %q, want its antigravity pin", got)
	}
	if got := testLaunchProfile(t, "codex", LaunchOptions{}).Provider; got != "openai" {
		t.Errorf("codex: got %q, want its openai record", got)
	}
	if got := testLaunchProfile(t, "opencode", LaunchOptions{Provider: "openai"}).Provider; got != "openai" {
		t.Errorf("flag: got %q, want openai", got)
	}
}

// The retired HARNESS_DATABRICKS_ARM env no longer beats the pin on a launch.
func TestInteractiveLaunchIgnoresRetiredArmEnv(t *testing.T) {
	hermetic(t, "")
	vaulttest.UseKeyring(t)
	row := func(n string) string {
		return `"` + n + `":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",` +
			`"auth":{"type":"key","command":"echo tok"},"models":{"opus":"databricks-claude-opus-5-5"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("gem-a")+`,`+row("arm")+`}}}`)
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "gemini", "profile", "gem-a")
	t.Setenv("HARNESS_DATABRICKS_ARM", "arm")
	if got := testLaunchProfile(t, "gemini", LaunchOptions{}).Provider; got != "gem-a" {
		t.Errorf("got %q, want the pin gem-a", got)
	}
}

func testLaunchProfile(t *testing.T, name string, o LaunchOptions) LaunchOptions {
	t.Helper()
	got, err := launchProfile(name, o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLaunchPlanSurfacesUnreadablePins(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	if _, err := LaunchPlan(context.Background(), "codex", LaunchOptions{PrintEnv: true}, nil); err == nil || !strings.Contains(err.Error(), "agent-records is corrupt") {
		t.Fatalf("LaunchPlan error = %v", err)
	}
}

// An untyped launch refuses a <profile>:<model> pin from the shell env, vault env or record before routing (D6).
func TestLaunchPlanRefusesPrefixedPin(t *testing.T) {
	for src, want := range map[string]string{"env": "env CODEX_MODEL", "vault env": "vault env CODEX_MODEL", "record": "codex record model"} {
		t.Run(src, func(t *testing.T) {
			vaulttest.Use(t)
			ctx := proc.WithValues(context.Background(), proc.Values{})
			switch src {
			case "env":
				ctx = proc.WithValues(ctx, proc.Values{"CODEX_MODEL": "dev:opus"})
			case "vault env":
				if err := providers.SetCustomEnv("CODEX_MODEL", "dev:opus"); err != nil {
					t.Fatal(err)
				}
			default:
				if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "dev:opus"}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := LaunchPlan(ctx, "codex", LaunchOptions{PrintEnv: true}, nil)
			if _, ok := errors.AsType[*UsageError](err); !ok || !errors.Is(err, providers.ErrModelSpec) || !strings.HasPrefix(err.Error(), want+": ") {
				t.Fatalf("err = %v, want a usage refusal naming %s", err, want)
			}
			if _, err := LaunchPlan(ctx, "codex", LaunchOptions{PrintEnv: true, Model: "gpt-5-5"}, nil); errors.Is(err, providers.ErrModelSpec) {
				t.Errorf("a typed model must outrank the pin: %v", err)
			}
		})
	}
}

func TestVendorSignInSkipsRouting(t *testing.T) {
	setConfigDir(t)
	t.Setenv("CLAUDE_PROFILE", "ultra") // neither codex nor gemini can use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	offers := 0
	OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { OfferConfigure = nil })
	for _, c := range []struct {
		vendor, bin string
		args        []string
	}{
		{"codex", "codex", []string{"login", "--device-auth"}},
		{"codex", "codex", []string{"logout"}},
		{"gemini", "agy", []string{"auth", "login"}},
	} {
		fake := pathstest.FakeVendor(t, c.bin, "")
		plan, err := LaunchPlan(context.Background(), c.vendor, LaunchOptions{}, c.args)
		if err != nil {
			t.Fatalf("%s %v: err = %v, want a plain sign-in launch", c.vendor, c.args, err)
		}
		if plan.Binary != fake || len(plan.Set) != 0 || !slices.Equal(plan.Argv[len(plan.Argv)-len(c.args):], c.args) {
			t.Errorf("%s %v: argv = %q, want the plain CLI with the args and no harness env", c.vendor, c.args, plan.Argv)
		}
	}
	if offers != 0 {
		t.Errorf("configure offered %d times for sign-in commands, want 0", offers)
	}
}

// A vendor argv that submits to the vendor's cloud is refused by RunPlan before any launch (--print-env included).
func TestRunPlanRefusesCloudSubmission(t *testing.T) {
	vaulttest.Use(t)
	tests := []struct {
		agent string
		argv  []string
		want  int
	}{
		{"claude", []string{"ultrareview", "main", "--post"}, 1},
		{"claude", []string{"--settings", "s.json", "ultrareview"}, 1},
		{"codex", []string{"-c", "m=1", "cloud", "exec", "x", "--env", "e"}, 1},
		{"codex", []string{"cloud", "status", "t1"}, 0},
		{"codex", []string{"exec", "ultrareview"}, 0},
		{"claude", []string{"-p", "hi"}, 0},
	}
	for _, tc := range tests {
		if rc := RunPlan(tc.agent, Plan{Binary: "vendor", Argv: tc.argv}, LaunchOptions{PrintEnv: true}, nil); rc != tc.want {
			t.Errorf("%s %v: rc=%d, want %d", tc.agent, tc.argv, rc, tc.want)
		}
	}
}

func TestLaunchRefusesOwnLoginAuto(t *testing.T) {
	setConfigDir(t)
	tb, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, c := range tb.OpencodeSub["opencode-go"] {
		id = c.Model
	}
	if id == "" {
		t.Fatalf("no opencode-go cell in %v", tb.OpencodeSub)
	}
	_, _, err = routeLaunch(t.Context(), "opencode", LaunchOptions{Model: "opencode-go/auto"})
	if err == nil || !strings.Contains(err.Error(), "auto is no opencode-go model; use one of ") || !strings.Contains(err.Error(), id) {
		t.Fatalf("opencode-go/auto = %v, want a refusal naming %s", err, id)
	}
	for _, m := range []string{"opencode-go/" + id, "auto"} {
		if _, _, err := routeLaunch(t.Context(), "opencode", LaunchOptions{Model: m}); err != nil && strings.Contains(err.Error(), "auto is no") {
			t.Errorf("%s refused as own-login auto: %v", m, err)
		}
	}
}

func TestPlainPlanRefusesAgentWithNoOwnLogin(t *testing.T) {
	_, err := plainPlan(Schema{Name: agentid.Pi}, nil)
	if usage, ok := err.(*UsageError); !ok || !strings.Contains(usage.Msg, "no login of its own") || !strings.Contains(usage.Msg, "--profile <dev|qa|prod>") {
		t.Fatalf("plainPlan(pi) = %v, want the no-own-login refusal", err)
	}
}

func TestPoolLaunchOnlyForUnpinnedNoOwnLogin(t *testing.T) {
	vaulttest.Use(t)
	o, pooled, err := poolLaunch(agentid.Pi, LaunchOptions{})
	if err != nil || !pooled || o.Model != AutoModel || o.Provider != poolName {
		t.Fatalf("unpinned pi = %+v %v %v, want the pool pick", o, pooled, err)
	}
	for _, in := range []struct {
		name string
		o    LaunchOptions
	}{{agentid.Pi, LaunchOptions{Provider: "dev"}}, {agentid.Pi, LaunchOptions{Model: "opus"}}, {agentid.Codex, LaunchOptions{}}} {
		if _, pooled, err := poolLaunch(in.name, in.o); pooled || err != nil {
			t.Errorf("%s %+v pooled = %v, %v", in.name, in.o, pooled, err)
		}
	}
}

func TestPoolPickedRefusalNamesTheWayForward(t *testing.T) {
	err := poolPicked(agentid.Pi, "", errors.New("no profile serves pi"))
	if err == nil || !strings.Contains(err.Error(), "claude-code pi --profile <dev|qa|prod>") || !strings.Contains(err.Error(), "claude-code databricks login") {
		t.Fatalf("err = %v", err)
	}
}

// Given a route overlay, ParseArgs folds it into the launch; a fallback key is refused naming the key.
func TestLaunchOverlayRoute(t *testing.T) {
	o, _, err := ParseArgs([]string{"--overlay", `{"provider":"databricks","profile":"dev","model":"opus"}`}, true)
	if err != nil || o.Provider != "dev" || o.Model != "opus" || o.Route.Provider != "databricks" {
		t.Fatalf("fold: %+v, %v", o, err)
	}
	if _, err := launchOverlay(o); err != nil {
		t.Fatalf("route overlay refused: %v", err)
	}
	if _, err := launchOverlay(LaunchOptions{Overlay: `{"fallback":"all"}`, PrintEnv: true}); err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("fallback: %v", err)
	}
}

func TestLaunchOverlayPoolKeepsTypedModel(t *testing.T) {
	vaulttest.Use(t)
	reg := routeRegistry(t)
	const wire = "databricks-claude-opus-5-5"
	for name, ready := range map[string]string{"dev": "READY", "qa": "READY", "prod": "NOT_READY"} {
		reg.Providers[name].Auth.Type = providers.AuthNone
		if err := router.SaveServed(router.Served{Profile: name, FetchedAt: time.Now(),
			Models: []router.ServedModel{{ID: wire, Task: "llm/v1/chat", Ready: ready}}}); err != nil {
			t.Fatal(err)
		}
	}
	claim := inflight.Claim{Owner: os.Getpid(), ID: "launch-overlay", Account: router.AccountOf(reg.Providers["dev"])}
	if ok, _ := inflight.TryClaim(claim, inflight.Limits{Total: 1}); !ok {
		t.Fatal("could not mark dev busy")
	}
	t.Cleanup(func() { inflight.Release(claim.Owner, claim.ID) })
	o, _, err := ParseArgs([]string{"--model", "opus", "--overlay",
		`{"provider":"databricks","profile":"auto","model":"sonnet"}`}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"qa", "dev"} {
		r, err := launchRoute(t.Context(), reg, "claude", o)
		if err != nil || r.Profile != want || r.Model != wire || o.Model != "opus" {
			t.Fatalf("typed %q: route = %+v, err = %v; want %s %s", o.Model, r, err, want, wire)
		}
		inflight.Release(claim.Owner, claim.ID)
	}
}
