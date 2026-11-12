package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
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

// --keep-env takes a comma list, repeatable, and accepts both the "--name value" and "--name=value" shapes.
func TestParseArgsKeepEnv(t *testing.T) {
	o, _, err := ParseArgs([]string{"--keep-env", "LANG,LC_ALL", "--keep-env=HOME"}, true)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"LANG", "LC_ALL", "HOME"}) {
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

func TestAgentLastProfileIsPerAgent(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "antigravity")
	vaulttest.SetRecord(t, "copilot", "profile", "antigravity")
	resolved := func(name string) string { return testLaunchProfile(t, name, LaunchOptions{}).Provider }
	providers.RecordLastProfile("claude", "anthropic")
	providers.RecordLastProfile("opencode", "openai")
	if got := resolved("opencode"); got != "openai" {
		t.Errorf("opencode entry: got %q, want openai (beats the antigravity pin)", got)
	}
	if got := resolved("copilot"); got != "antigravity" {
		t.Errorf("copilot read another agent's entry: got %q, want its antigravity pin", got)
	}
	if got := testLaunchProfile(t, "opencode", LaunchOptions{Provider: "antigravity"}).Provider; got != "antigravity" {
		t.Errorf("flag: got %q, want antigravity", got)
	}
	if err := providers.ClearLastProfilesFor("opencode"); err != nil {
		t.Fatal(err)
	}
	if got := resolved("opencode"); got != "antigravity" {
		t.Errorf("after the opencode pin setter: got %q, want the antigravity pin", got)
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
			t.Errorf("%s %v: plan = %+v, want the plain CLI with the args and no harness env", c.vendor, c.args, plan)
		}
	}
	if offers != 0 {
		t.Errorf("configure offered %d times for sign-in commands, want 0", offers)
	}
}
