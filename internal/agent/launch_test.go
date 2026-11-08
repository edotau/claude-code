package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

// Flags beat the agent's profile pin; a pin fills what the flag left empty; AGENT_ACTIVE=off ignores it.
func TestWithAgentPinsPrecedence(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "copilot", "profile", "max")
	vaulttest.SetRecord(t, "copilot", "model", "kimi-k3")
	if got := testAgentPins(t, "copilot", LaunchOptions{}); got.Provider != "max" || got.Model != "" {
		t.Errorf("pins only = %+v, want max and no model (the model pin applies after the target resolves)", got)
	}
	if got := testAgentPins(t, "copilot", LaunchOptions{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if got := testAgentPins(t, "copilot", LaunchOptions{}); got.Provider != "" || got.Model != "" {
		t.Errorf("COPILOT_ACTIVE=off = %+v, want the pins ignored", got)
	}
}

// An opencode record pinned to opencode-go (valid via PinIDs, no registry row) resolves to its own plan, not an error.
func TestPinnedProfileOpencodeGo(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	got, err := pinnedProfile("opencode")
	if err != nil || got != "" {
		t.Errorf("pinnedProfile(opencode) = %q, %v; want empty profile and no error", got, err)
	}
	vaulttest.SetRecord(t, "opencode", "profile", "no-such-profile")
	if _, err := pinnedProfile("opencode"); err == nil {
		t.Error("an invalid pin must still be an error")
	}
}

const pinDbxRow = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"pat"}}}}}`

// A model pin follows the pinned profile into the target; the flag beats it; a Databricks id is dropped off Databricks.
func TestAgentModelTargetPin(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", pinDbxRow)
	const m = "databricks-gpt-6-1-sol"
	vaulttest.SetRecord(t, "codex", "profile", "dev")
	vaulttest.SetRecord(t, "codex", "model", m)
	if got := testAgentPins(t, "codex", LaunchOptions{}); got.Provider != "dev" || got.Model != "" {
		t.Fatalf("codex = %+v, want the profile only", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := providers.Target{Provider: reg.Providers["dev"], Model: "base"}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: dev}); got.Model != m {
		t.Errorf("dev = %q, want %s kept", got.Model, m)
	}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Flag: "flag-m", Target: dev}); got.Model == m {
		t.Errorf("explicit --model lost to the pin: %q", got.Model)
	}
	off := providers.Target{Provider: &providers.Provider{Name: "chatgpt"}, Model: "base"}
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: off}); got.Model != "base" {
		t.Errorf("chatgpt = %q, want the databricks id skipped", got.Model)
	}
}

// A vendor the harness cannot route still launches its own CLI untouched, after one configure offer.
func TestVendorPassesThroughWhenUnroutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_PROFILE", "max") // passthrough auth: gemini cannot use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	fake := pathstest.FakeVendor(t, "agy", "")
	offers := 0
	OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { OfferConfigure = nil })

	plan, err := LaunchPlan(context.Background(), "gemini", LaunchOptions{}, []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("expected a passthrough plan, got %v", err)
	}
	// gemini's own built-in vendor spec (vendorconfig defaults) still prepends --dangerously-skip-permissions.
	wantArgv := []string{"--dangerously-skip-permissions", "-p", "hi"}
	if plan.Binary != fake || !slices.Equal(plan.Argv, wantArgv) || len(plan.Set) != 0 {
		t.Errorf("plan = %+v, want the plain CLI with %v prepended and no harness env", plan, wantArgv)
	}
	if offers != 1 {
		t.Errorf("configure offered %d times, want 1", offers)
	}
	// --print-env never prompts.
	offers = 0
	if _, err := LaunchPlan(context.Background(), "gemini", LaunchOptions{PrintEnv: true}, nil); err != nil || offers != 0 {
		t.Errorf("print-env: err=%v offers=%d", err, offers)
	}
}

// A vendor's own sign-in skips route resolution and the configure offer: the login is what makes it routable.
func TestVendorSignInSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "max") // neither codex nor gemini can use it
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
			t.Errorf("%s %v: plan = %+v, want the plain CLI with the args and no harness env", c.vendor, c.args, plan)
		}
	}
	if offers != 0 {
		t.Errorf("configure offered %d times for sign-in commands, want 0", offers)
	}
}

// An own-login launch keeps an explicit --model whether PreRoute or Configure selects that route.
func TestOwnLoginLaunchPreservesModelFlag(t *testing.T) {
	const model = "opencode-go/kimi-k3.1-pro"
	cases := []struct{ name, auth, profile string }{
		{"pre-route", `{"opencode":{"type":"api"}}`, ""},
		{"configure", `{"github-copilot":{"type":"oauth"}}`, "personal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t, "")
			pathstest.FakeVendor(t, "opencode", "")
			dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(tc.auth), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := LaunchPlan(t.Context(), "opencode", LaunchOptions{
				Model: model, Provider: tc.profile, PrintEnv: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"--model", model}; !slices.Equal(plan.Argv, want) {
				t.Errorf("Argv = %v, want %v", plan.Argv, want)
			}
		})
	}
}

func TestPlainPlansResolveVendorBinNotPATH(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			source := vendorPATHFixture(t, paths.Vendors[name], "")
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

// plainPlan must apply the vendor spec (env + prepended args) on every own-login/passthrough launch.
func TestPlainPlanAppliesVendorLaunchSpec(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	fake := pathstest.FakeVendor(t, "opencode", "")

	spec, ok := LookupSchema("opencode")
	if !ok {
		t.Fatal("opencode vendor spec not found")
	}

	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X_TEST=1"],"args":["--flag"]}}}}`)
	plan, err := plainPlan(spec, []string{"user", "arg"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if plan.Set["X_TEST"] != "1" {
		t.Errorf("Set[X_TEST] = %q, want 1", plan.Set["X_TEST"])
	}
	wantArgv := []string{"--flag", "user", "arg"}
	if !slices.Equal(plan.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v (spec args prepended)", plan.Argv, wantArgv)
	}

	// No overlay: the built-in default still applies.
	vaulttest.Put(t, vault.Overlay, "agents", "")
	plan, err = plainPlan(spec, []string{"user"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != fake {
		t.Errorf("Binary = %q, want %q", plan.Binary, fake)
	}
	if plan.Set["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"] != "1" {
		t.Errorf("Set[OPENCODE_DISABLE_CLAUDE_CODE_SKILLS] = %q, want 1 (built-in default)", plan.Set["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS"])
	}
	if !slices.Equal(plan.Argv, []string{"user"}) {
		t.Errorf("Argv = %v, want [user] (opencode's default carries no Launch.Args)", plan.Argv)
	}
}

func vendorPATHFixture(t *testing.T, name, body string) string {
	t.Helper()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := pathstest.FakeVendor(t, name, body)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if name == "agy" {
		dst := filepath.Join(filepath.Dir(bin), "agy")
		if err := os.Rename(bin, dst); err != nil {
			t.Fatal(err)
		}
		bin = dst
	}
	return bin
}

const parityRows = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}}},` +
	`"anthropic":{"alt":{"base_url":"https://alt.example.com","protocol":"anthropic","auth":{"type":"none"},"models":{"opus":"claude-opus-5-5"}}}}}`

var codexModelArg = regexp.MustCompile(`^model="(.*)"$`)

// launchModel is the model the interactive launch hands the CLI (--print-env plan): codex's -c model=, else COPILOT_MODEL.
func launchModel(t *testing.T, name string, o LaunchOptions) string {
	t.Helper()
	o.PrintEnv = true
	plan, err := LaunchPlan(context.Background(), name, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := plan.Set["COPILOT_MODEL"]; m != "" {
		return m
	}
	for i, a := range plan.Argv {
		if m := codexModelArg.FindStringSubmatch(a); m != nil && i > 0 && plan.Argv[i-1] == "-c" {
			return m[1]
		}
		if a == "--model" && i+1 < len(plan.Argv) {
			return plan.Argv[i+1]
		}
	}
	t.Fatalf("no model in %v", plan.Argv)
	return ""
}

// The interactive launch and the headless dispatch pick the same model for the same agent, profile, flag and pin.
func TestLaunchAndAgentAgreeOnModel(t *testing.T) {
	cases := []struct{ agent, profile, flag string }{
		{"codex", "dev", ""},
		{"codex", "dev", "gpt-5.5"},
		{"copilot", "alt", ""},
		{"copilot", "alt", "claude-sonnet-5-5"},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.profile+"/"+c.flag, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", parityRows)
			pathstest.FakeVendor(t, c.agent, "")
			pin := "databricks-gpt-6-1-sol"
			if c.agent == "copilot" {
				pin = "claude-haiku-5-5"
			}
			vaulttest.SetRecord(t, c.agent, "model", pin)
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			tg, _, _, err := SchemaFor(c.agent).targetNote(RunOptions{Provider: c.profile, Model: c.flag})
			if err != nil {
				t.Fatal(err)
			}
			got := launchModel(t, c.agent, LaunchOptions{Provider: c.profile, Model: c.flag})
			if got != tg.Model {
				t.Errorf("launch %q vs agent %q", got, tg.Model)
			}
			if c.flag == "" && !strings.Contains(got, pin) {
				t.Errorf("model %q ignores the pin %q", got, pin)
			}
		})
	}
}

func testAgentPins(t *testing.T, name string, o LaunchOptions) LaunchOptions {
	t.Helper()
	got, err := withAgentPins(name, o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const (
	testRouterBase   = "http://127.0.0.1:9999"
	testRouterSecret = "rtr-secret-xyz"
)

func stubRouter(t *testing.T, ensure func(context.Context) (string, error)) {
	t.Helper()
	oe, os_ := ensureRouter, routerSecret
	t.Cleanup(func() { ensureRouter, routerSecret = oe, os_ })
	if ensure != nil {
		ensureRouter = ensure
	}
}

func cfgFor(t *testing.T, ctx context.Context, vendor, provider, model string) ([]string, []string) {
	t.Helper()
	env, argv, err := SchemaFor(vendor).Configure(ctx, target(t, provider, model), "")
	if err != nil {
		t.Fatal(err)
	}
	return env, argv
}

func TestRoutedStrips1MSuffix(t *testing.T) {
	stubRouter(t, nil)
	env, _ := cfgFor(t, context.Background(), "copilot", "anthropic", "claude-opus-5[1m]")
	if m := envMap(env)["COPILOT_MODEL"]; m != "claude-opus-5" {
		t.Errorf("COPILOT_MODEL = %q", m)
	}
}

func TestLaunchEnvCannotOverrideRouterBase(t *testing.T) {
	stubRouter(t, nil)
	tg := target(t, "openai", "gpt-5")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"copilot":{"launch":{"env":["COPILOT_PROVIDER_BASE_URL=https://evil.example.com","KEEP_ME=1"]}}}}`)
	env, _, err := SchemaFor("copilot").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL=") {
			n++
			if !strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL="+testRouterBase) {
				t.Errorf("base overridden: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("base var appears %d times: %q", n, env)
	}
	if envMap(env)["KEEP_ME"] != "1" {
		t.Errorf("unrelated launch.env entry dropped: %q", env)
	}
}

func TestAuthNoneNotRouted(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) {
		t.Error("router ensured for AuthNone")
		return "", errors.New("x")
	})
	tg := target(t, "openai", "gpt-5")
	p := *tg.Provider
	p.Auth = providers.Auth{Type: providers.AuthNone}
	tg.Provider = &p
	env, argv, err := SchemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), testRouterBase) || strings.Contains(strings.Join(env, " "), testRouterSecret) {
		t.Errorf("AuthNone was routed: %q %q", env, argv)
	}
}

func TestEnsureRouterErrorSurfaces(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("boom") })
	_, _, err := SchemaFor("codex").Configure(context.Background(), target(t, "openai", "gpt-5"), "")
	if err == nil || !strings.Contains(err.Error(), "router") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

const agentHdr = "X-Claude-Code-Agent"

// withHeaders returns tg with the provider's own headers set.
func withHeaders(tg providers.Target, h map[string]string) providers.Target {
	p := *tg.Provider
	p.Headers = h
	tg.Provider = &p
	return tg
}

// A direct dispatch never passes routerRoute, so it carries the provider's own headers and no agent tag.
func TestDirectDispatchesHaveNoAgentTag(t *testing.T) {
	stubRouter(t, nil)
	vaulttest.Use(t)
	ctx := WithDirect(context.Background())
	own := map[string]string{"X-Tenant": "t1"}

	_, argv, err := SchemaFor("codex").Configure(ctx, withHeaders(target(t, "openai", "gpt-5"), own), "")
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(argv, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
		t.Errorf("direct codex headers wrong: %q", argv)
	}
	for _, v := range []string{"opencode", "copilot"} {
		env, _, err := SchemaFor(v).Configure(ctx, withHeaders(target(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		if j := strings.Join(env, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
			t.Errorf("direct %s headers wrong: %q", v, env)
		}
	}
}

// D4: gemini (no Dialects) is refused on any non-login row, even one with a gemini route.
func TestGeminiRefusedOnRoutedRow(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "gem-key", Models: map[string]string{"opus": "gemini-x"},
		Routes: map[string]string{providers.ProtocolGemini: "https://g.example.com"}}
	if CanServe("gemini", p) {
		t.Error("CanServe(gemini) = true on a keyed row with a gemini route")
	}
	if !CanServe("opencode", p) {
		t.Error("control: opencode speaks gemini and should serve the row")
	}
	_, _, err := SchemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}

// Interactive codex (nil Headless) carries the CODEX_EFFORT pin; headless Effort still wins.
func TestCodexInteractiveCarriesEffortPin(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.SetRecord(t, "codex", "effort", "medium")
	_, argv, err := SchemaFor("codex").Configure(context.Background(), tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("interactive argv = %q, want the effort pin", argv)
	}
	_, argv, err = SchemaFor("codex").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="medium"`) {
		t.Errorf("headless argv = %q, want explicit low over the pin", argv)
	}
}

// configureRoute surfaces Serves' reason verbatim, prefixed by the agent.
func TestConfigureRouteUsesServesReason(t *testing.T) {
	vaulttest.Use(t)
	ant := &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}
	cases := []struct {
		agent string
		p     *providers.Provider
	}{
		{"gemini", ant},
		{"codex", ant},
		{"copilot", &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}},
	}
	for _, c := range cases {
		_, why := Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := SchemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}

// Given a claude entry, an agent CLI launch ignores it; its own entry beats the agent pin, and the pin setter clears only it.
func TestAgentLastProfileIsPerAgent(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "nvidia")
	vaulttest.SetRecord(t, "copilot", "profile", "nvidia")
	resolved := func(name string) string { return testLaunchProfile(t, name, LaunchOptions{}).Provider }
	providers.RecordLastProfile("claude", "anthropic")
	providers.RecordLastProfile("opencode", "openai")
	if got := resolved("opencode"); got != "openai" {
		t.Errorf("opencode entry: got %q, want openai (beats the nvidia pin)", got)
	}
	if got := resolved("copilot"); got != "nvidia" {
		t.Errorf("copilot read another agent's entry: got %q, want its nvidia pin", got)
	}
	if got := testLaunchProfile(t, "opencode", LaunchOptions{Provider: "nvidia"}).Provider; got != "nvidia" {
		t.Errorf("flag: got %q, want nvidia", got)
	}
	if err := providers.ClearLastProfilesFor("opencode"); err != nil {
		t.Fatal(err)
	}
	if got := resolved("opencode"); got != "nvidia" {
		t.Errorf("after the opencode pin setter: got %q, want the nvidia pin", got)
	}
	if got := providers.LastProfile("claude"); got != "anthropic" {
		t.Errorf("claude entry lost by the opencode setter: %q", got)
	}
}

// Given codex records, its launch reuses the repo's last profile (own-login chatgpt included) and an off codex stays off.
func TestCodexReusesLastProfileButOffStaysOff(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	providers.RecordLastProfile("codex", "chatgpt")
	if got := testLaunchProfile(t, "codex", LaunchOptions{}).Provider; got != "chatgpt" {
		t.Errorf("on codex: got %q, want the repo's last-used chatgpt over its record", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{}); err != nil {
		t.Fatal(err)
	}
	if got := testLaunchProfile(t, "codex", LaunchOptions{}).Provider; got != "" {
		t.Errorf("off codex: got %q, the cache must never turn it on", got)
	}
}

// Given a gemini last-used gateway row over a different vault pin, a gemini launch resolves flag > last-used > pin.
func TestGeminiReusesLastProfile(t *testing.T) {
	hermetic(t, "")
	vaulttest.UseKeyring(t)
	row := func(n string) string {
		return `"` + n + `":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",` +
			`"auth":{"type":"key","command":"echo tok"},"models":{"opus":"databricks-claude-opus-5-5"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("gem-a")+`,`+row("gem-b")+`}}}`)
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "gemini", "profile", "gem-a")
	providers.RecordLastProfile("gemini", "gem-b")
	if got := testLaunchProfile(t, "gemini", LaunchOptions{}).Provider; got != "gem-b" {
		t.Errorf("gemini last-used: got %q, want gem-b over the gem-a pin", got)
	}
	if got := testLaunchProfile(t, "gemini", LaunchOptions{Provider: "gem-a"}).Provider; got != "gem-a" {
		t.Errorf("flag: got %q, want gem-a", got)
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
