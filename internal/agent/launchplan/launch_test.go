package launchplan_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/launchplan"
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
		if got := launchplan.InteractiveArgs(tc.args); got != tc.want {
			t.Errorf("interactiveVendorArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestWithAgentPinsPrecedence(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "copilot", "profile", "max")
	vaulttest.SetRecord(t, "copilot", "model", "kimi-k3")
	if got := testAgentPins(t, "copilot", agent.LaunchOptions{}); got.Provider != "max" || got.Model != "" {
		t.Errorf("pins only = %+v, want max and no model (the model pin applies after the target resolves)", got)
	}
	if got := testAgentPins(t, "copilot", agent.LaunchOptions{Provider: "personal", Model: "m"}); got.Provider != "personal" || got.Model != "m" {
		t.Errorf("flags = %+v, want flags to beat the pins", got)
	}
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if got := testAgentPins(t, "copilot", agent.LaunchOptions{}); got.Provider != "" || got.Model != "" {
		t.Errorf("COPILOT_ACTIVE=off = %+v, want the pins ignored", got)
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
	if got := testAgentPins(t, "codex", agent.LaunchOptions{}); got.Provider != "dev" || got.Model != "" {
		t.Fatalf("codex = %+v, want the profile only", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := providers.Target{Provider: reg.Providers["dev"], Model: "base"}
	if got, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "codex", Target: dev}); got.Model != m {
		t.Errorf("dev = %q, want %s kept", got.Model, m)
	}
	if got, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "codex", Flag: "flag-m", Target: dev}); got.Model == m {
		t.Errorf("explicit --model lost to the pin: %q", got.Model)
	}
	off := providers.Target{Provider: &providers.Provider{Name: "chatgpt"}, Model: "base"}
	if got, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "codex", Target: off}); got.Model != "base" {
		t.Errorf("chatgpt = %q, want the databricks id skipped", got.Model)
	}
}

func TestVendorPassesThroughWhenUnroutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_PROFILE", "max") // passthrough auth: gemini cannot use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	fake := pathstest.FakeVendor(t, "agy", "")
	offers := 0
	launchplan.OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { launchplan.OfferConfigure = nil })

	plan, err := launchplan.LaunchPlan(context.Background(), "gemini", agent.LaunchOptions{}, []string{"-p", "hi"})
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
	if _, err := launchplan.LaunchPlan(context.Background(), "gemini", agent.LaunchOptions{PrintEnv: true}, nil); err != nil || offers != 0 {
		t.Errorf("print-env: err=%v offers=%d", err, offers)
	}
}

func TestConfigureDeclinedPreservesModelFlag(t *testing.T) {
	hermetic(t, "")
	fake := pathstest.FakeVendor(t, "agy", "")
	const model = "gemini-3.8-flash-high"
	o, args, err := launchplan.ParseArgs([]string{"--profile", "antigravity", "--model", model, "-p", "hi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	offers := 0
	original := launchplan.OfferConfigure
	launchplan.OfferConfigure = func(name string, reason error) bool {
		offers++
		if name != "gemini" || !strings.Contains(reason.Error(), "not signed in") {
			t.Errorf("configure offer = %q, %v; want missing Gemini login", name, reason)
		}
		return false
	}
	t.Cleanup(func() { launchplan.OfferConfigure = original })
	plan, err := launchplan.LaunchPlan(t.Context(), "gemini", o, args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--model", model, "-p", "hi"}
	if offers != 1 || plan.Binary != fake || len(plan.Set) != 0 || !slices.Equal(plan.Argv, want) {
		t.Fatalf("offers=%d plan=%+v; want plain CLI with argv %v", offers, plan, want)
	}
}

func TestConfigureRetryPreservesAgentModelPin(t *testing.T) {
	hermetic(t, "")
	fake := pathstest.FakeVendor(t, "agy", "")
	const model = "gemini-3.8-flash-low"
	vaulttest.SetRecord(t, "gemini", "model", model)
	offers := 0
	original := launchplan.OfferConfigure
	launchplan.OfferConfigure = func(name string, reason error) bool {
		offers++
		if name != "gemini" || !strings.Contains(reason.Error(), "not signed in") {
			t.Errorf("configure offer = %q, %v; want missing Gemini login", name, reason)
		}
		marker := filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("test-login"), 0o600); err != nil {
			t.Fatal(err)
		}
		return true
	}
	t.Cleanup(func() { launchplan.OfferConfigure = original })
	plan, err := launchplan.LaunchPlan(t.Context(), "gemini", agent.LaunchOptions{Provider: "antigravity"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", model, "--dangerously-skip-permissions", "-p", "hi"}
	if offers != 1 || plan.Binary != fake || !slices.Equal(plan.Argv, want) {
		t.Fatalf("offers=%d plan=%+v; want configured CLI with pinned model in argv %v", offers, plan, want)
	}
}

func TestVendorSignInSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "max") // neither codex nor gemini can use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	offers := 0
	launchplan.OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { launchplan.OfferConfigure = nil })
	for _, c := range []struct {
		vendor, bin string
		args        []string
	}{
		{"codex", "codex", []string{"login", "--device-auth"}},
		{"codex", "codex", []string{"logout"}},
		{"gemini", "agy", []string{"auth", "login"}},
	} {
		fake := pathstest.FakeVendor(t, c.bin, "")
		plan, err := launchplan.LaunchPlan(context.Background(), c.vendor, agent.LaunchOptions{}, c.args)
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
			plan, err := launchplan.LaunchPlan(t.Context(), "opencode", agent.LaunchOptions{
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
	for _, name := range agent.Names() {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			source := vendorPATHFixture(t, paths.Vendors[name], "")
			t.Setenv("PATH", filepath.Dir(source))
			bin := pathstest.FakeVendor(t, name, "")
			spec, ok := agent.LookupSchema(name)
			if !ok {
				t.Fatalf("missing vendor %s", name)
			}
			plan, err := launchplan.PlainPlan(spec, nil)
			if err != nil || plan.Binary != bin {
				t.Fatalf("plainPlan = %q, %v; want %q", plan.Binary, err, bin)
			}
			if err := os.Remove(bin); err != nil {
				t.Fatal(err)
			}
			_, err = launchplan.PlainPlan(spec, nil)
			if err == nil || !strings.Contains(err.Error(), "not installed at "+bin) {
				t.Fatalf("missing vendor: %v; want bin-dir error despite PATH vendor", err)
			}
		})
	}
}

func TestPlainPlanAppliesVendorLaunchSpec(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	fake := pathstest.FakeVendor(t, "opencode", "")

	spec, ok := agent.LookupSchema("opencode")
	if !ok {
		t.Fatal("opencode vendor spec not found")
	}

	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X_TEST=1"],"args":["--flag"]}}}}`)
	plan, err := launchplan.PlainPlan(spec, []string{"user", "arg"})
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
	plan, err = launchplan.PlainPlan(spec, []string{"user"})
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

func launchModel(t *testing.T, name string, o agent.LaunchOptions) string {
	t.Helper()
	o.PrintEnv = true
	plan, err := launchplan.LaunchPlan(context.Background(), name, o, nil)
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
			tg, _, err := agent.SchemaFor(c.agent).Target(agent.RunOptions{Provider: c.profile, Model: c.flag})
			if err != nil {
				t.Fatal(err)
			}
			got := launchModel(t, c.agent, agent.LaunchOptions{Provider: c.profile, Model: c.flag})
			if got != tg.Model {
				t.Errorf("launch %q vs agent %q", got, tg.Model)
			}
			if c.flag == "" && !strings.Contains(got, pin) {
				t.Errorf("model %q ignores the pin %q", got, pin)
			}
		})
	}
}

func testAgentPins(t *testing.T, name string, o agent.LaunchOptions) agent.LaunchOptions {
	t.Helper()
	got, err := launchplan.WithAgentPins(name, o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAgentLastProfileIsPerAgent(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "nvidia")
	vaulttest.SetRecord(t, "copilot", "profile", "nvidia")
	resolved := func(name string) string { return testLaunchProfile(t, name, agent.LaunchOptions{}).Provider }
	providers.RecordLastProfile("claude", "anthropic")
	providers.RecordLastProfile("opencode", "openai")
	if got := resolved("opencode"); got != "openai" {
		t.Errorf("opencode entry: got %q, want openai (beats the nvidia pin)", got)
	}
	if got := resolved("copilot"); got != "nvidia" {
		t.Errorf("copilot read another agent's entry: got %q, want its nvidia pin", got)
	}
	if got := testLaunchProfile(t, "opencode", agent.LaunchOptions{Provider: "nvidia"}).Provider; got != "nvidia" {
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

func TestCodexReusesLastProfileButOffStaysOff(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	providers.RecordLastProfile("codex", "chatgpt")
	if got := testLaunchProfile(t, "codex", agent.LaunchOptions{}).Provider; got != "chatgpt" {
		t.Errorf("on codex: got %q, want the repo's last-used chatgpt over its record", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{}); err != nil {
		t.Fatal(err)
	}
	if got := testLaunchProfile(t, "codex", agent.LaunchOptions{}).Provider; got != "" {
		t.Errorf("off codex: got %q, the cache must never turn it on", got)
	}
}

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
	if got := testLaunchProfile(t, "gemini", agent.LaunchOptions{}).Provider; got != "gem-b" {
		t.Errorf("gemini last-used: got %q, want gem-b over the gem-a pin", got)
	}
	if got := testLaunchProfile(t, "gemini", agent.LaunchOptions{Provider: "gem-a"}).Provider; got != "gem-a" {
		t.Errorf("flag: got %q, want gem-a", got)
	}
}

// The retired HARNESS_DATABRICKS_ARM env no longer beats the last-used profile on a launch.
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
	if got := testLaunchProfile(t, "gemini", agent.LaunchOptions{}).Provider; got != "gem-a" {
		t.Errorf("got %q, want the pin gem-a", got)
	}
}

func testLaunchProfile(t *testing.T, name string, o agent.LaunchOptions) agent.LaunchOptions {
	t.Helper()
	got, err := launchplan.LaunchProfile(name, o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLaunchPlanSurfacesUnreadablePins(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	if _, err := launchplan.LaunchPlan(context.Background(), "codex", agent.LaunchOptions{PrintEnv: true}, nil); err == nil || !strings.Contains(err.Error(), "agent-records is corrupt") {
		t.Fatalf("LaunchPlan error = %v", err)
	}
}
