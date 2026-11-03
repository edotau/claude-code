package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
		if got := agent.IsHeadless(tc.args); got != tc.want {
			t.Errorf("agent.IsHeadless(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// hermetic isolates config, pins and seams; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	for _, kv := range os.Environ() { // the launching shell's own session env must not steer the plan
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") || strings.HasPrefix(k, "HARNESS_") {
			t.Setenv(k, "")
		}
	}
	dir := vaulttest.Use(t)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{providers.EnvSessionProfile, "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	oldFind, oldSelf := findClaude, selfBinary
	findClaude = func() (string, error) { return "/opt/claude", nil }
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	agent.StubRouterForTest(t, "http://127.0.0.1:4000")
	t.Cleanup(func() { findClaude, selfBinary = oldFind, oldSelf })
}

func TestParseArgs(t *testing.T) {
	o, rest, err := agent.ParseArgs([]string{"--profile=openai", "-p", "hi", "--model", "gpt-5", "--print-env", "--", "--model", "x"}, true)
	if err != nil || o.Provider != "openai" || o.Model != "gpt-5" || !o.PrintEnv {
		t.Fatalf("%+v %v", o, err)
	}
	if !slices.Equal(rest, []string{"-p", "hi", "--model", "x"}) {
		t.Errorf("rest %q", rest)
	}
	if _, _, err := agent.ParseArgs([]string{"--profile"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// --keep-env takes a comma list, repeatable, and accepts both the "--name value" and "--name=value" shapes.
func TestParseArgsKeepEnv(t *testing.T) {
	o, _, err := agent.ParseArgs([]string{"--keep-env", "DATABRICKS_HOST,DATABRICKS_TOKEN", "--keep-env=HOME"}, true)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Fatalf("KeepEnv = %q, err %v", o.KeepEnv, err)
	}
	if _, _, err := agent.ParseArgs([]string{"--keep-env"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

func TestConfigDirEnvIsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for dir, want := range map[string]bool{"": false, filepath.Join(home, ".claude"): true,
		filepath.Join(home, ".claude") + "/": true, filepath.Join(home, "wt"): false} {
		t.Setenv("CLAUDE_CONFIG_DIR", dir)
		if got := configDirEnvIsDefault(); got != want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: got %v, want %v", dir, got, want)
		}
	}
}

// gwProviders is a direct (non-router) anthropic-protocol gateway keyed by TEST_GW_KEY.
const gwProviders = `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
	"auth": {"type":"key", "env": "TEST_GW_KEY"}, "headers": {"X-B": "2", "X-A": "1"},
	"models": {"opus": "claude-opus-4-6", "haiku": "claude-haiku-4-5"}}}}}`

// Direct route: the provider's own base URL, sorted headers and apiKeyHelper overlay; the key never enters the plan.
func TestClaudeDirectLaunchPlan(t *testing.T) {
	hermetic(t, gwProviders)
	t.Setenv("TEST_GW_KEY", fakeToken)
	before, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example/anthropic", "ANTHROPIC_CUSTOM_HEADERS": "X-A: 1\nX-B: 2",
		"ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4-6", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4-5",
		"MAX_THINKING_TOKENS": "31999", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING": "1",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS": "300000", providers.EnvSessionProfile: "gw"}
	for k, v := range want {
		if p.Set[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Set[k], v)
		}
	}
	if !slices.Contains(p.Unset, oauthKey) || !slices.Equal(p.Argv, []string{"--settings", overlayFile(p).Path, "-p", "hi"}) {
		t.Errorf("unset %q argv %q", p.Unset, p.Argv)
	}
	if !strings.Contains(string(overlayFile(p).Data), `"/opt/bin/claude-code resolve --profile gw"`) {
		t.Errorf("overlay %s", overlayFile(p).Data)
	}
	var buf bytes.Buffer
	p.Print(&buf)
	if strings.Contains(buf.String()+string(overlayFile(p).Data)+strings.Join(p.Argv, " "), fakeToken) {
		t.Errorf("the key reached the plan:\n%s", buf.String())
	}
	after, err := providers.Load()
	if err != nil || !reflect.DeepEqual(before.Providers["gw"], after.Providers["gw"]) {
		t.Errorf("registry provider mutated by the launch: %v", err)
	}
}

// Routed route: an openai-protocol profile goes through the loopback router, carrying only the agent header.
func TestClaudeRoutedLaunchPlan(t *testing.T) {
	hermetic(t, `{"providers": {"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:11434/v1",
		"auth": {"type": "none"}, "models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`)
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "ollama"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000/p/ollama", "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen3-coder",
		"ANTHROPIC_CUSTOM_HEADERS": "X-Claude-Code-Agent: claude", "CLAUDE_CODE_DISABLE_1M_CONTEXT": "1", providers.EnvSessionProfile: "ollama"}
	for k, v := range want {
		if p.Set[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Set[k], v)
		}
	}
	if m, ok := p.Set["ANTHROPIC_MODEL"]; ok {
		t.Errorf("no --model asked, yet ANTHROPIC_MODEL = %q", m)
	}
	if !strings.Contains(string(overlayFile(p).Data), "claude-code resolve --proxy") || !strings.HasSuffix(overlayFile(p).Path, "overlay-ollama-proxy.json") {
		t.Errorf("overlay %s at %s", overlayFile(p).Data, overlayFile(p).Path)
	}
}

// Print masks credential-named env values and quoted credential-shaped strings in argv, and keeps the rest readable.
func TestClaudePlanPrintRedactsSecrets(t *testing.T) {
	hermetic(t, gwProviders)
	t.Setenv("TEST_GW_KEY", fakeToken)
	args := []string{"-p", `{"x-key" = "` + fakeToken + `"}`}
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "gw"}, args)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Print(&buf)
	out := buf.String()
	if strings.Contains(out, fakeToken) || strings.Contains(out, "FAKEFAKE") {
		t.Errorf("Print leaked a secret:\n%s", out)
	}
	for _, w := range []string{`"<redacted>"`, "set      ANTHROPIC_CUSTOM_HEADERS=<redacted>", "ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-6"} {
		if !strings.Contains(out, w) {
			t.Errorf("Print missing %q:\n%s", w, out)
		}
	}
}

func TestClaudeAnthropicDefaultOmitsBaseURL(t *testing.T) {
	hermetic(t, `{"fallback": []}`)
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "anthropic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Set["ANTHROPIC_BASE_URL"]; ok {
		t.Error("api.anthropic.com should not be pinned as ANTHROPIC_BASE_URL")
	}
	if len(p.Warnings) == 0 {
		t.Error("expected a missing-credential warning")
	}
}

func TestClaudePassthrough(t *testing.T) {
	hermetic(t, "")
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "max"}, []string{"--resume"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 0 || slices.Contains(p.Argv, "--settings") {
		t.Errorf("passthrough must not set an apiKeyHelper: %q %s", p.Argv, overlayFile(p).Data)
	}
	// Interactive: the setup-token is withheld so Remote Control sees the /login credential.
	if !slices.Contains(p.Unset, oauthKey) || p.Set[oauthKey] != "" {
		t.Error("interactive passthrough must withhold CLAUDE_CODE_OAUTH_TOKEN")
	}
	t.Setenv(oauthKey, "sk-ant-oat-test")
	h, err := Claude(context.Background(), agent.LaunchOptions{Provider: "max"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.Unset, oauthKey) {
		t.Error("headless passthrough must keep CLAUDE_CODE_OAUTH_TOKEN")
	}
	// The built-in fallback does not route a login: a loopback base URL would disable Remote Control.
	if _, ok := p.Set["ANTHROPIC_BASE_URL"]; ok || p.Set["ANTHROPIC_CUSTOM_HEADERS"] != "" {
		t.Errorf("passthrough must launch direct: base %q headers %q", p.Set["ANTHROPIC_BASE_URL"], p.Set["ANTHROPIC_CUSTOM_HEADERS"])
	}
	if _, err := Claude(context.Background(), agent.LaunchOptions{Provider: "chatgpt"}, nil); err == nil || !strings.Contains(err.Error(), "claude-code codex") {
		t.Errorf("a vendor-login provider must not back Claude Code: %v", err)
	}
}

// --model takes an alias (the alias's own resolution, so pins apply), a version or a wire id; the provider:model
// form is gone, and the verb's alias stays the main model unless --model names another family.
func TestClaudeModelFlag(t *testing.T) {
	hermetic(t, `{"providers": {"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:11434/v1", "auth": {"type": "none"},
		"models": {"opus": "qwen3-coder", "sonnet": "qwen3-sonnet", "haiku": "qwen3-haiku"}}}}}`)
	for _, tc := range []struct {
		name string
		o    agent.LaunchOptions
		want map[string]string
	}{
		{"alias keeps the alias's own model", agent.LaunchOptions{Provider: "ollama", Model: "sonnet"},
			map[string]string{"ANTHROPIC_MODEL": "qwen3-sonnet", "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen3-coder"}},
		{"verb alias with a wire id fills that alias", agent.LaunchOptions{Provider: "ollama", Alias: "sonnet", Model: "qwen3:30b"},
			map[string]string{"ANTHROPIC_MODEL": "qwen3:30b", "ANTHROPIC_DEFAULT_SONNET_MODEL": "qwen3:30b", "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen3-coder"}},
		{"bare wire id fills opus", agent.LaunchOptions{Provider: "ollama", Model: "qwen3-x"},
			map[string]string{"ANTHROPIC_MODEL": "qwen3-x", "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen3-x"}},
		{"a family overrides the verb alias", agent.LaunchOptions{Provider: "ollama", Alias: "haiku", Model: "claude-sonnet-5-5"},
			map[string]string{"ANTHROPIC_MODEL": "claude-sonnet-5-5", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "qwen3-haiku"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Claude(context.Background(), tc.o, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.want {
				if p.Set[k] != v {
					t.Errorf("%s = %q, want %q (set %v)", k, p.Set[k], v, p.Set)
				}
			}
			if p.Set[providers.EnvSessionProfile] != "ollama" {
				t.Errorf("%s = %q, want ollama", providers.EnvSessionProfile, p.Set[providers.EnvSessionProfile])
			}
		})
	}
}

// The overlay is shared across sessions, so a scratch build must not become every session's apiKeyHelper.
func TestHelperBinPrefersInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if got := helperBin("/tmp/scratch/claude-code"); got != "/tmp/scratch/claude-code" {
		t.Errorf("nothing installed: %q", got)
	}
	bin := filepath.Join(dir, "bin", "claude-code")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := helperBin("/tmp/scratch/claude-code"); got != bin {
		t.Errorf("installed binary must win: %q", got)
	}
}

// Every launch, routed or direct, exports CLAUDE_PROFILE so the session's statusline stays on its own provider.
func TestLaunchExportsHarnessProvider(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
		"auth": {"type":"key", "env": "TEST_GW_KEY"}, "models": {"opus": "claude-opus-4-6"}}},
		"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:11434/v1", "auth": {"type": "none"},
		"models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`)
	t.Setenv("TEST_GW_KEY", "secret-value")
	for _, name := range []string{"gw", "ollama"} { // direct, routed
		p, err := Claude(context.Background(), agent.LaunchOptions{Provider: name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Set[providers.EnvSessionProfile]; got != name {
			t.Errorf("%s: %s = %q, want %q", name, providers.EnvSessionProfile, got, name)
		}
	}
}

// A shell ANTHROPIC_MODEL/BASE_URL beats the vault model pin and survives the wipe; credentials are still scrubbed.
func TestClaudeShellEndpointBeatsVault(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
		"auth": {"type":"key", "env": "TEST_GW_KEY"}, "models": {"opus": "claude-opus-4-6"}}}}}`)
	t.Setenv("TEST_GW_KEY", "k")
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_MODEL", "shell-model")
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	env := p.Env([]string{"ANTHROPIC_MODEL=shell-model", "ANTHROPIC_BASE_URL=https://shell.example", "ANTHROPIC_AUTH_TOKEN=stale"})
	for _, w := range []string{"ANTHROPIC_MODEL=shell-model", "ANTHROPIC_BASE_URL=https://shell.example"} {
		if !slices.Contains(env, w) {
			t.Errorf("env lost %s: %q", w, env)
		}
	}
	if slices.Contains(env, "ANTHROPIC_AUTH_TOKEN=stale") {
		t.Error("credential survived the scrub")
	}
}

func TestClaudeShellEndpointGetsNoProfileCredential(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
		"auth": {"type":"key", "env": "TEST_GW_KEY"}, "models": {"opus": "claude-opus-4-6"}}}}}`)
	t.Setenv("TEST_GW_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	p, err := Claude(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 0 || slices.Contains(p.Argv, "--settings") {
		t.Errorf("foreign ANTHROPIC_BASE_URL still gets the profile apiKeyHelper: argv %q overlay %s", p.Argv, overlayFile(p).Data)
	}
	if _, ok := p.Set["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("foreign ANTHROPIC_BASE_URL still gets the harness headers")
	}
	if !sameEndpoint("https://GW.example/anthropic/", "https://gw.example/anthropic") || sameEndpoint("https://gw.example:8443", "https://gw.example") {
		t.Error("sameEndpoint must ignore host case and a trailing slash, but not a port")
	}
}

// TestAnthropicOAuthWithheldFromShellEndpoint: --oauth must not inject the vault token for a foreign shell base URL.
func TestAnthropicOAuthWithheldFromShellEndpoint(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"max": {"protocol": "anthropic", "provider": "anthropic",
		"base_url": "https://api.anthropic.com", "auth": {"type":"cli"}, "models": {"opus": "claude-opus-4-6"}}}}}`)
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	o, rest, err := ParseAnthropicArgs([]string{"--oauth", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Anthropic(context.Background(), o, rest, "vault-oauth-token")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Set {
		if v == "vault-oauth-token" {
			t.Fatalf("vault OAuth token sent to a foreign shell endpoint under %s", k)
		}
	}
}

// TestKeepShellEndpointDefaultBase: a shell base equal to the default Anthropic endpoint is not foreign when the plan sets none.
func TestKeepShellEndpointDefaultBase(t *testing.T) {
	for _, tc := range []struct {
		base    string
		foreign bool
	}{{"https://api.anthropic.com", false}, {"https://API.anthropic.com/", false}, {"https://shell.example", true}} {
		t.Setenv("ANTHROPIC_BASE_URL", tc.base)
		plan := agent.Plan{Set: map[string]string{}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}}, Argv: []string{"--settings", "/tmp/ov.json"}}
		keepShellEndpoint(&plan, agent.LaunchOptions{})
		if plan.ForeignBase != tc.foreign || (len(plan.Files) == 0) != tc.foreign {
			t.Errorf("base %q: ForeignBase=%v files=%v, want foreign=%v", tc.base, plan.ForeignBase, plan.Files, tc.foreign)
		}
	}
}

// TestKeepShellEndpointInheritedProxyBase: an enclosing session's /p/<other> base must not displace this launch's own proxy base.
func TestKeepShellEndpointInheritedProxyBase(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/prod")
	plan := agent.Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:18765/p/dev"}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}},
		Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(&plan, agent.LaunchOptions{})
	if plan.ForeignBase || len(plan.Files) == 0 || plan.Set["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:18765/p/dev" {
		t.Errorf("inherited proxy base won: foreign=%v files=%v set=%q", plan.ForeignBase, plan.Files, plan.Set["ANTHROPIC_BASE_URL"])
	}
	if slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Error("inherited proxy base kept through the scrub")
	}
}

// TestKeepShellEndpointOtherPortProxyBase: a loopback /p/ base on another port is the user's override, not inherited.
func TestKeepShellEndpointOtherPortProxyBase(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9999/p/prod")
	plan := agent.Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:18765/p/dev"}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}},
		Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(&plan, agent.LaunchOptions{})
	if !plan.ForeignBase || len(plan.Files) != 0 || !slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Errorf("other-port base overridden: foreign=%v files=%v keep=%v", plan.ForeignBase, plan.Files, plan.Keep)
	}
}

// overlayFile is the plan's settings overlay, or the zero file when it has none.
func overlayFile(p agent.Plan) agent.LaunchFile {
	if len(p.Files) == 0 {
		return agent.LaunchFile{}
	}
	return p.Files[0]
}
