package agent

import (
	"context"
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
			tg, _, _, err := schemaFor(c.agent).targetNote(RunOptions{Provider: c.profile, Model: c.flag})
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
