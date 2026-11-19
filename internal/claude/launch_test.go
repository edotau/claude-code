package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain runs in the hermetic host (hosttest), so an inherited COWORKER_OVERLAY or HARNESS_* pin never steers a plan.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// hermetic isolates config, pins and seams; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	for _, kv := range os.Environ() { // the launching shell's own session env must not steer the plan
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") || strings.HasPrefix(k, "HARNESS_") {
			t.Setenv(k, "")
		}
	}
	hosttest.HermeticEnv(t)
	vault.DisableSyncForTests()
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	oldFind, oldSelf := findClaude, selfBinary
	findClaude = func() (string, error) { return "/opt/claude", nil }
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	t.Cleanup(agent.StubRouter("http://127.0.0.1:4000"))
	stubMint(t, func(context.Context, []string, int) (string, error) { return "cct_HERMETIC", nil })
	t.Cleanup(func() { findClaude, selfBinary = oldFind, oldSelf })
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
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example/anthropic", "ANTHROPIC_CUSTOM_HEADERS": "X-A: 1\nX-B: 2",
		"ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4-6", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4-5",
		"MAX_THINKING_TOKENS": "31999", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING": "1",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS": "300000", providers.EnvSessionProfile: "gw"}
	assertDirectLaunchEnv(t, p, want)
	if !slices.Contains(p.Unset, oauthKey) || !slices.Equal(p.Argv, []string{"--settings", overlayFile(p).Path, "-p", "hi"}) {
		t.Errorf("unset %q argv %q", p.Unset, p.Argv)
	}
	if !strings.Contains(string(overlayFile(p).Data), `"/opt/bin/claude-code resolve --secret --profile gw"`) {
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

func assertDirectLaunchEnv(t *testing.T, p agent.Plan, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if p.Set[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Set[k], v)
		}
	}
}

// Routed route: an openai-protocol profile goes through the loopback router, carrying only the agent header.
func TestClaudeRoutedLaunchPlan(t *testing.T) {
	hermetic(t, `{"providers": {"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:11434/v1",
		"auth": {"type": "none"}, "models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`)
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "ollama"}, nil)
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
	if !strings.Contains(string(overlayFile(p).Data), "claude-code resolve --secret --proxy") || !strings.HasSuffix(overlayFile(p).Path, "overlay-ollama-proxy.json") {
		t.Errorf("overlay %s at %s", overlayFile(p).Data, overlayFile(p).Path)
	}
}

// A headless dispatch's identity rides the proxy headers so the proxy's credential mint applies it.
func TestClaudeRoutedLaunchCarriesIdentity(t *testing.T) {
	hermetic(t, `{"providers": {"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:11434/v1",
		"auth": {"type": "none"}, "models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`)
	ctx := providers.WithIdentity(context.Background(), providers.IdentityHeadless)
	p, err := claudePlan(ctx, agent.LaunchOptions{Provider: "ollama"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Set["ANTHROPIC_CUSTOM_HEADERS"], "X-Claude-Code-Agent: claude\nX-Claude-Code-Identity: headless"; got != want {
		t.Errorf("headers = %q, want %q", got, want)
	}
}

// Print masks credential-named env values and quoted credential-shaped strings in argv, and keeps the rest readable.
func TestClaudePlanPrintRedactsSecrets(t *testing.T) {
	hermetic(t, gwProviders)
	t.Setenv("TEST_GW_KEY", fakeToken)
	args := []string{"-p", `{"x-key" = "` + fakeToken + `"}`}
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "gw"}, args)
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
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "anthropic"}, nil)
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
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "ultra"}, []string{"--resume"})
	if err != nil {
		t.Fatal(err)
	}
	if hasKeyHelper(p) {
		t.Errorf("passthrough must not set an apiKeyHelper: %q %s", p.Argv, overlayFile(p).Data)
	}
	// Interactive: the setup-token is withheld so Remote Control sees the /login credential.
	if !slices.Contains(p.Unset, oauthKey) || p.Set[oauthKey] != "" {
		t.Error("interactive passthrough must withhold CLAUDE_CODE_OAUTH_TOKEN")
	}
	t.Setenv(oauthKey, "sk-ant-oat-test")
	h, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "ultra"}, []string{"-p", "hi"})
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
	t.Run("reject vendor login", testClaudeRejectsVendorLogin)
}

func testClaudeRejectsVendorLogin(t *testing.T) {
	t.Helper()
	if _, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "chatgpt"}, nil); err == nil || !strings.Contains(err.Error(), "claude-code codex") {
		t.Errorf("a vendor-login provider must not back Claude Code: %v", err)
	}
}

// --model takes an alias (the alias's own resolution, so pins apply), a version or a wire id
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
			p, err := claudePlan(context.Background(), tc.o, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.want {
				if p.Set[k] != v {
					t.Errorf("%s = %q, want %q (set %d)", k, p.Set[k], v, len(p.Set))
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
		p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: name}, nil)
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
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	env := p.Env([]string{"ANTHROPIC_MODEL=shell-model", "ANTHROPIC_BASE_URL=https://shell.example", "ANTHROPIC_AUTH_TOKEN=stale"})
	for _, w := range []string{"ANTHROPIC_MODEL=shell-model", "ANTHROPIC_BASE_URL=https://shell.example"} {
		if !slices.Contains(env, w) {
			t.Errorf("env lost %s: %d", w, len(env))
		}
	}
	if slices.Contains(env, "ANTHROPIC_AUTH_TOKEN=stale") {
		t.Error("credential survived the scrub")
	}
}

func TestClaudeShellEndpointDropsInheritedOAuth(t *testing.T) {
	hermetic(t, "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	t.Setenv(oauthKey, "test-inherited-oauth")
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "ultra"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	env := p.Env(os.Environ())
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=https://shell.example") {
		t.Fatal("foreign shell endpoint was not preserved")
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, oauthKey+"=") {
			t.Fatal("inherited OAuth token reached the foreign endpoint child environment")
		}
	}
}

func TestClaudeShellEndpointGetsNoProfileCredential(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
		"auth": {"type":"key", "env": "TEST_GW_KEY"}, "models": {"opus": "claude-opus-4-6"}}}}}`)
	t.Setenv("TEST_GW_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "gw"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if hasKeyHelper(p) {
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
	hermetic(t, `{"fallback": [], "providers": {"anthropic": {"ultra": {"protocol": "anthropic", "provider": "anthropic",
		"base_url": "https://api.anthropic.com", "auth": {"type":"cli"}, "models": {"opus": "claude-opus-4-6"}}}}}`)
	t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example")
	o, rest, err := parseAnthropicArgs([]string{"--oauth", "-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := anthropicPlan(context.Background(), o, rest, "vault-oauth-token")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Set {
		if v == "vault-oauth-token" {
			t.Fatalf("vault OAuth token sent to a foreign shell endpoint under %s", k)
		}
	}
}

// TestKeepShellEndpointDefaultBase: a shell base equal to the default Anthropic endpoint is not foreign.
func TestKeepShellEndpointDefaultBase(t *testing.T) {
	for _, tc := range []struct {
		base    string
		foreign bool
	}{{"https://api.anthropic.com", false}, {"https://API.anthropic.com/", false}, {"https://shell.example", true}} {
		t.Setenv("ANTHROPIC_BASE_URL", tc.base)
		plan := agent.Plan{Set: map[string]string{}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}}, Argv: []string{"--settings", "/tmp/ov.json"}}
		keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
		if plan.ForeignBase != tc.foreign || (len(plan.Files) == 0) != tc.foreign {
			t.Errorf("base %q: ForeignBase=%v files=%v, want foreign=%v", tc.base, plan.ForeignBase, plan.Files, tc.foreign)
		}
	}
}

// TestKeepShellEndpointInheritedProxyBase: an enclosing session's /p/<other> base never displaces this launch's own.
func TestKeepShellEndpointInheritedProxyBase(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/prod")
	plan := agent.Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:18765/p/dev"}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}},
		Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
	if plan.ForeignBase || len(plan.Files) == 0 || plan.Set["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:18765/p/dev" {
		t.Errorf("inherited proxy base won: foreign=%v files=%v set=%q", plan.ForeignBase, plan.Files, plan.Set["ANTHROPIC_BASE_URL"])
	}
	if slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Error("inherited proxy base kept through the scrub")
	}
}

// TestKeepShellEndpointDispatchTaggedBase: a launch under a /d/<id>-tagged parent base keeps its proxy credential.
func TestKeepShellEndpointDispatchTaggedBase(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/d/20261113T222949.044-claude/p/dev")
	plan := agent.Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:18765/p/dev"}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}},
		Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
	if plan.ForeignBase || len(plan.Files) == 0 || slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Errorf("tagged parent base read as foreign: foreign=%v files=%v keep=%v", plan.ForeignBase, plan.Files, plan.Keep)
	}
}

// TestKeepShellEndpointOtherPortProxyBase: a loopback /p/ base on another port is the user's override, not inherited.
func TestKeepShellEndpointOtherPortProxyBase(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9999/p/prod")
	plan := agent.Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:18765/p/dev"}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}},
		Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
	if !plan.ForeignBase || len(plan.Files) != 0 || !slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Errorf("other-port base overridden: foreign=%v files=%v keep=%v", plan.ForeignBase, plan.Files, plan.Keep)
	}
}

// TestClaudePlanReadsRunSnapshot: a plan decides the shell endpoint from its run snapshot, not the process env.
func TestClaudePlanReadsRunSnapshot(t *testing.T) {
	hermetic(t, gwProviders)
	for _, tc := range []struct {
		name, ambient, run string
		foreign            bool
	}{
		{"snapshot base is foreign", "", "https://shell.example", true},
		{"ambient base is not the run's", "https://shell.example", "", false},
	} {
		t.Setenv("ANTHROPIC_BASE_URL", tc.ambient)
		ctx := proc.WithValues(context.Background(), proc.Values{"ANTHROPIC_BASE_URL": tc.run})
		p, err := claudePlan(ctx, agent.LaunchOptions{Provider: "gw"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p.ForeignBase != tc.foreign || slices.Contains(p.Keep, "ANTHROPIC_BASE_URL") != tc.foreign {
			t.Errorf("%s: ForeignBase=%v keep=%v, want foreign=%v", tc.name, p.ForeignBase, p.Keep, tc.foreign)
		}
	}
}

func overlayFile(p agent.Plan) agent.LaunchFile {
	if len(p.Files) == 0 {
		return agent.LaunchFile{}
	}
	return p.Files[0]
}

// TestKeepShellEndpointSessionProxyBaseToFirstParty
func TestKeepShellEndpointSessionProxyBaseToFirstParty(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "prod")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/prod")
	orig := readProxyState
	readProxyState = func() (gateway.State, error) { return gateway.State{Port: 18765}, nil }
	t.Cleanup(func() { readProxyState = orig })
	plan := agent.Plan{Set: map[string]string{}, Files: []agent.LaunchFile{{Path: "/tmp/ov.json"}}, Argv: []string{"--settings", "/tmp/ov.json"}}
	keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
	if plan.ForeignBase || len(plan.Files) == 0 || slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Errorf("session proxy base kept: foreign=%v files=%v keep=%v", plan.ForeignBase, plan.Files, plan.Keep)
	}
}

// From a direct Databricks session (serving route, no proxy) an ultra launch drops that base and keeps its own login.
func TestKeepShellEndpointSessionRouteBaseToFirstParty(t *testing.T) {
	hermetic(t, `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	t.Setenv("CLAUDE_PROFILE", "qa")
	t.Setenv("ANTHROPIC_BASE_URL", "https://qa.cloud.databricks.com/serving-endpoints/anthropic/")
	plan := agent.Plan{Set: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	keepShellEndpoint(context.Background(), &plan, agent.LaunchOptions{})
	if plan.ForeignBase || plan.Set["CLAUDE_CODE_OAUTH_TOKEN"] == "" || slices.Contains(plan.Keep, "ANTHROPIC_BASE_URL") {
		t.Errorf("session route base kept: foreign=%v token=%v keep=%v", plan.ForeignBase, plan.Set["CLAUDE_CODE_OAUTH_TOKEN"] != "", plan.Keep)
	}
}

// An interactive plan carries the record's effort, launch.args and launch.env; a typed --effort replaces the record's.
func TestClaudeInteractiveRendersRecord(t *testing.T) {
	const dbx = `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`
	rec := vault.AgentRecord{Effort: "high", Launch: &vault.AgentLaunch{Env: []string{"FOO_LAUNCH=1",
		"ANTHROPIC_SMALL_FAST_MODEL=elsewhere"}, Args: []string{"--launch-arg"}}}
	for _, prof := range []string{"ultra", "qa"} {
		hermetic(t, dbx)
		plan := func(args ...string) agent.Plan {
			p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: prof}, args)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		checkArgv(t, prof+" no record", plan().Argv, nil, []string{"--effort", "--launch-arg"})
		if err := vault.SetAgentRecord("claude", rec); err != nil {
			t.Fatal(err)
		}
		p := plan()
		checkArgv(t, prof+" record", p.Argv, []string{"--launch-arg --effort high"}, []string{"--permission"})
		if p.Set["FOO_LAUNCH"] != "1" || p.Set["ANTHROPIC_SMALL_FAST_MODEL"] != "" {
			t.Errorf("%s: set %d, want FOO_LAUNCH=1 and no wiped launch.env name", prof, len(p.Set))
		}
		checkArgv(t, prof+" typed", plan("--effort=low").Argv, []string{"--launch-arg --effort=low"}, []string{"--effort high"})
		checkArgv(t, prof+" prompt", plan("--", "--effort").Argv, []string{"--effort high -- --effort"}, nil)
		checkArgv(t, prof+" headless", plan("--print").Argv, nil, []string{"--effort", "--launch-arg"})
	}
}

// Bare `anthropic` (no --profile) goes through claudePlan(), so a refused record is a warning there too.
func TestAnthropicRendersRecord(t *testing.T) {
	hermetic(t, "")
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	p, err := anthropicPlan(context.Background(), anthropicOptions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, "anthropic", p.Argv, []string{"--effort high"}, nil)
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Effort: "turbo"}); err != nil {
		t.Fatal(err)
	}
	if p, err = anthropicPlan(context.Background(), anthropicOptions{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, "refused record", p.Argv, nil, []string{"--effort"})
	if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "stored agent settings not applied") }) {
		t.Errorf("refused record: warnings %q, want the not-applied notice", p.Warnings)
	}
}

// An invalid stored record on an interactive launch applies the built-ins with exactly one notice, even in --print-env.
func TestClaudeInteractiveRefusedRecordOneNotice(t *testing.T) {
	hermetic(t, "")
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Effort: "turbo"}); err != nil {
		t.Fatal(err)
	}
	var notices bytes.Buffer
	orig := agent.NoticeOut
	agent.NoticeOut = &notices
	t.Cleanup(func() { agent.NoticeOut = orig })
	p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "ultra"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, "refused record", p.Argv, nil, []string{"--effort"})
	var shown bytes.Buffer
	p.Print(&shown)
	const want = "stored agent settings not applied: "
	if n := strings.Count(notices.String(), want) + strings.Count(shown.String(), want); n != 1 {
		t.Errorf("notice shown %d times (stderr %q, print-env %q), want once", n, notices.String(), shown.String())
	}
	if len(p.Warnings) != 1 || !strings.HasPrefix(p.Warnings[0], "claude "+want) || !strings.HasSuffix(p.Warnings[0], "; built-in defaults used") {
		t.Errorf("warnings %q, want one launchSettings-shaped notice", p.Warnings)
	}
}

// armDBX is two workspaces behind a fallback ladder, so a dev launch goes through the loopback proxy.
const armDBX = `{"fallback":["qa"],"providers":{"databricks":{
	"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}},
	"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// A proxied child holds a session token bound to this pid, never the secret: unarmed {dev, fallback qa}, armed {dev}.
func TestClaudeProxyCarriesSessionToken(t *testing.T) {
	hermetic(t, armDBX)
	const tok = "cct_ARM_TEST"
	var minted []string
	stubMint(t, func(_ context.Context, profiles []string, pid int) (string, error) {
		minted = append(minted, strings.Join(profiles, ","))
		if pid != os.Getpid() {
			t.Errorf("token bound to pid %d, want this process %d", pid, os.Getpid())
		}
		return tok, nil
	})
	for _, c := range []struct{ name, overlay, mint string }{
		{"unarmed", "", "dev,qa"},
		{"armed", `{"provider":"databricks","profile":"dev"}`, "dev"},
	} {
		t.Setenv("COWORKER_OVERLAY", c.overlay)
		agent.ResetSessionTokens()
		minted = nil
		p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "dev"}, []string{"-p", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Set[ProxyKeyEnv]; got != tok || !slices.Equal(minted, []string{c.mint}) {
			t.Errorf("%s: %s is the minted token = %v (len %d), mints %v, want [%s]", c.name, ProxyKeyEnv, got == tok,
				len(got), minted, c.mint)
		}
		assertProxiedNoSecret(t, c.name, p)
	}
}

// assertProxiedNoSecret: p routes /p/dev through the resolve --proxy helper and no env value holds the router secret.
func assertProxiedNoSecret(t *testing.T, name string, p agent.Plan) {
	t.Helper()
	secret, err := gateway.ClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	helper := string(overlayFile(p).Data)
	if base := p.Set["ANTHROPIC_BASE_URL"]; base != "http://127.0.0.1:4000/p/dev" || !strings.Contains(helper, "resolve --secret --proxy") {
		t.Errorf("%s: base = %q, overlay %s; want /p/dev and the resolve --secret --proxy helper", name, base, helper)
	}
	for k, v := range p.Set {
		if strings.Contains(v, secret) {
			t.Errorf("%s: %s carries the router secret", name, k)
		}
	}
}

// An alias on another profile widens an unarmed token to that profile; armed, the token stays on the confined one.
func TestClaudeProxyTokenCoversAliasProfiles(t *testing.T) {
	hermetic(t, strings.Replace(armDBX, `"fallback":["qa"]`, `"fallback":[]`, 1)) // qa is reachable only as an alias
	var minted []string
	stubMint(t, func(_ context.Context, profiles []string, _ int) (string, error) {
		minted = append(minted, strings.Join(profiles, ","))
		return "cct_SCOPED", nil
	})
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev := reg.Providers["dev"]
	sel := providers.Selection{Provider: dev, Aliases: map[string]providers.Target{
		models.Opus: {Provider: dev, Model: "databricks-claude-opus-5-5"},
		"sonnet":    {Provider: reg.Providers["qa"], Model: "databricks-claude-sonnet-5"},
	}}
	for _, c := range []struct{ name, overlay, mint string }{
		{"unarmed", "", "dev,qa"},
		{"armed", `{"provider":"databricks","profile":"dev"}`, "dev"},
	} {
		t.Setenv("COWORKER_OVERLAY", c.overlay)
		agent.ResetSessionTokens()
		minted = nil
		plan := agent.Plan{Set: map[string]string{}}
		if _, _, err := configureClaudeProxy(t.Context(), sel, dev, false, "/opt/bin/claude-code", &plan); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(minted, []string{c.mint}) || plan.Set[ProxyKeyEnv] != "cct_SCOPED" {
			t.Errorf("%s: mints %v, token set %v; want [%s]", c.name, minted, plan.Set[ProxyKeyEnv] != "", c.mint)
		}
	}
}

// A failed mint aborts the launch; the child is never handed the router secret instead.
func TestClaudeProxyMintFailureAbortsLaunch(t *testing.T) {
	hermetic(t, armDBX)
	stubMint(t, func(context.Context, []string, int) (string, error) {
		return "", errors.New("too many live session tokens")
	})
	_, err := claudePlan(t.Context(), agent.LaunchOptions{Provider: "dev"}, []string{"-p", "hi"})
	if err == nil || !strings.Contains(err.Error(), "too many live session tokens") {
		t.Fatalf("claudePlan err = %v, want the mint failure", err)
	}
}

// An interactive proxied launch warns that Remote Control is lost; a headless one stays quiet.
func TestClaudeProxiedInteractiveWarnsRemoteControl(t *testing.T) {
	hermetic(t, armDBX)
	const warn = "Remote Control is unavailable through the proxy; launch a direct login session to keep it"
	for _, tc := range []struct {
		args []string
		want bool
	}{{nil, true}, {[]string{"-p", "hi"}, false}} {
		p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "dev"}, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(p.Warnings, warn); got != tc.want {
			t.Errorf("args %q: warned = %v, want %v (warnings %q)", tc.args, got, tc.want, p.Warnings)
		}
	}
}

// The auto-mode classifier model is the provider-qualified opus id, not Claude Code's unserved default.
func TestSetModelAliasEnvPinsAutoModeModel(t *testing.T) {
	hermetic(t, armDBX)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "dev")
	if err != nil {
		t.Fatal(err)
	}
	plan := agent.Plan{Set: map[string]string{}}
	setModelAliasEnv(&plan, sel)
	want := sel.ClientModel(models.Opus)
	got := plan.Set["CLAUDE_CODE_AUTO_MODE_MODEL"]
	if !strings.HasPrefix(got, "databricks-") || !strings.Contains(got, "opus") || got != want {
		t.Errorf("auto-mode model = %q, want opus id %q", got, want)
	}
}

// stubMint swaps agent.MintSession for mint over an empty session-token cache, restoring both after t.
func stubMint(t *testing.T, mint func(context.Context, []string, int) (string, error)) {
	orig := agent.MintSession
	agent.MintSession = mint
	agent.ResetSessionTokens()
	t.Cleanup(func() { agent.MintSession = orig; agent.ResetSessionTokens() })
}

// Armed, every claude launch in one process shares one session token per profile, so a batch never fills the cap.
func TestClaudeArmLaunchesMintOnce(t *testing.T) {
	hermetic(t, armDBX)
	mints := 0
	stubMint(t, func(context.Context, []string, int) (string, error) { mints++; return "cct_ONCE", nil })
	t.Setenv("COWORKER_OVERLAY", `{"provider":"databricks","profile":"dev"}`)
	for range 3 {
		p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "dev"}, []string{"-p", "hi"})
		if err != nil || p.Set[ProxyKeyEnv] != "cct_ONCE" {
			t.Fatalf("armed launch: err %v, session token set %v", err, p.Set[ProxyKeyEnv] != "")
		}
	}
	if mints != 1 {
		t.Errorf("3 armed launches minted %d session tokens, want 1", mints)
	}
}

// Every dev launch goes through /p/dev on a session token, armed or not.
func TestClaudeArmHeadlessAlwaysProxied(t *testing.T) {
	hermetic(t, `{"fallback":[],"providers":{"databricks":{"dev":{"preset":"databricks",
		"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	stubMint(t, func(context.Context, []string, int) (string, error) { return "cct_ARM_dev", nil })
	launch := func(args ...string) agent.Plan {
		p, err := claudePlan(context.Background(), agent.LaunchOptions{Provider: "dev"}, args)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	const proxied = "http://127.0.0.1:4000/p/dev"
	if p := launch("-p", "hi"); p.Set["ANTHROPIC_BASE_URL"] != proxied || p.Set[ProxyKeyEnv] != "cct_ARM_dev" {
		t.Errorf("unarmed headless base = %q, want %s with the session token", p.Set["ANTHROPIC_BASE_URL"], proxied)
	}
	t.Setenv("COWORKER_OVERLAY", `{"provider":"databricks","profile":"dev"}`)
	p := launch("-p", "hi")
	if p.Set["ANTHROPIC_BASE_URL"] != proxied || p.Set[ProxyKeyEnv] != "cct_ARM_dev" {
		t.Errorf("armed headless: base %q, session token set %v; want /p/dev and the token", p.Set["ANTHROPIC_BASE_URL"], p.Set[ProxyKeyEnv] != "")
	}
	if !strings.Contains(string(overlayFile(p).Data), "resolve --secret --proxy") {
		t.Errorf("armed headless helper %s, want resolve --secret --proxy", overlayFile(p).Data)
	}
	if p := launch(); p.Set["ANTHROPIC_BASE_URL"] != proxied || p.Set[ProxyKeyEnv] != "cct_ARM_dev" {
		t.Errorf("armed interactive base = %q, session token set %v; want %s and the token", p.Set["ANTHROPIC_BASE_URL"], p.Set[ProxyKeyEnv] != "", proxied)
	}
}

// An armed login session routed through the proxy sends the session token, not the router secret, in its own header.
func TestClaudeArmPassthroughHeaderIsSessionToken(t *testing.T) {
	hermetic(t, armDBX)
	const tok = "cct_ARM_TEST"
	stubMint(t, func(context.Context, []string, int) (string, error) { return tok, nil })
	t.Setenv("COWORKER_OVERLAY", `{"provider":"databricks","profile":"dev"}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	ultra := reg.Providers["ultra"]
	plan := agent.Plan{Set: map[string]string{}}
	if _, _, err := configureClaudeProxy(context.Background(), providers.Selection{Provider: ultra}, ultra, true, "/opt/bin/claude-code", &plan); err != nil {
		t.Fatal(err)
	}
	if hdrs := plan.Set["ANTHROPIC_CUSTOM_HEADERS"]; !strings.Contains(hdrs, gateway.ClientHeader+": "+tok) {
		t.Errorf("armed passthrough: the client header carries the session token = false (headers len %d)", len(hdrs))
	}
}

func TestClaudeBinaryPrefersPATHOverVersions(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	versions := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0.9", "2.0.14"} {
		if err := os.WriteFile(filepath.Join(versions, version), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := pathstest.FakeVendorOutsideConfig(t, "claude", "")
	t.Setenv("PATH", filepath.Dir(bin))
	got, err := ClaudeBinary()
	if err != nil || got != bin {
		t.Fatalf("ClaudeBinary = %q, %v; want %q", got, err, bin)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(versions, "2.0.14")
	got, err = ClaudeBinary()
	if err != nil || got != want {
		t.Fatalf("without a PATH claude = %q, %v; want %q", got, err, want)
	}
}

func TestClaudeBinarySkipsBinDirLauncher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	launcher := pathstest.FakeVendor(t, "claude", "")
	t.Setenv("PATH", filepath.Dir(launcher))
	if bin, err := ClaudeBinary(); err == nil || !strings.Contains(err.Error(), "not found on PATH") || bin != "" {
		t.Fatalf("ClaudeBinary = %q, %v; want not found despite the bin/claude launcher", bin, err)
	}
}

func TestClaudeBinaryBinDirOverrideSkipsInstalledVersions(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	versions := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versions, "2.0.14"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.AgentBinDirEnv, t.TempDir())
	if got, err := ClaudeBinary(); err == nil || got != "" {
		t.Fatalf("ClaudeBinary = %q, %v; want error, no fallback to the installed claude", got, err)
	}
}

func TestTakeAsBeatsProfileIdentity(t *testing.T) {
	as, rest, err := providers.TakeAs([]string{"--model", "x", "--as", "sp", "--", "--as", "user"})
	if err != nil || as != providers.IdentitySP {
		t.Fatalf("as=%q err=%v", as, err)
	}
	if len(rest) != 5 || rest[3] != "--as" {
		t.Fatalf("rest=%v", rest)
	}
	// the flag wins over a profile set to user
	if got, _ := providers.ResolveIdentity(providers.IdentityUser, as); got != providers.IdentitySP {
		t.Fatalf("resolved %q", got)
	}
	if _, _, err := providers.TakeAs([]string{"--as=bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	if err := agent.ValidateAs("sp", "codex"); err == nil {
		t.Fatal("--as accepted for codex")
	}
}

// `<alias> --help` runs claude's own help; a profile with no model for the alias must not refuse it.
func TestLaunchClaudeHelpSkipsModelLookup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeNativeClaude(t, `[ "$1" = --help ] && echo FAKE-CLAUDE-HELP`)
	code, out, errOut := hosttest.CaptureOutput(t, func() int { return Launch("nosuchalias", []string{"--help"}) })
	if code != 0 || !strings.Contains(out, "FAKE-CLAUDE-HELP") {
		t.Fatalf("Launch(--help) = %d, stdout %q, stderr %q; want 0 and claude's help", code, out, errOut)
	}
}

// Only a lone help flag takes the shortcut; a subcommand's --help goes through to claude.
func TestBareHelpKeepsSubcommandHelp(t *testing.T) {
	for args, want := range map[string]bool{"--help": true, "-h": true, "mcp --help": false, "-p hi --help": false} {
		if got := bareHelp(strings.Fields(args)); got != want {
			t.Errorf("bareHelp(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestNoFableAliasEnvWithoutFableModel(t *testing.T) {
	hermetic(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://example.cloud.databricks.com",
		"base_url": "https://example.cloud.databricks.com/serving-endpoints/anthropic", "auth": {"type": "none"},
		"models": {"opus": "databricks-claude-opus-5-5"}}},
		"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic", "auth": {"type": "none"},
		"models": {"opus": "claude-opus-4-6"}}}}}`)
	for _, c := range []struct{ profile, want string }{
		{"dbx", ""},
		{"gw", ""},
	} {
		sel, err := providers.Select(mustRegistry(t), c.profile)
		if err != nil {
			t.Fatal(err)
		}
		plan := agent.Plan{Set: map[string]string{}}
		setModelAliasEnv(&plan, sel)
		if got := plan.Set["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != c.want {
			t.Errorf("%s: ANTHROPIC_DEFAULT_FABLE_MODEL = %q, want %q", c.profile, got, c.want)
		}
	}
}

func mustRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// pickerFixture has dev/qa signed in (auth none), prod without a credential (pat, no secret).
const pickerFixture = `{"providers":{"databricks":{
	"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"none"}},
	"prod":{"preset":"databricks","host":"https://prod.cloud.databricks.com","auth":{"type":"pat"}},
	"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"none"}}}}}`

func pickerModels(t *testing.T, p agent.Plan) []pickerRow {
	t.Helper()
	var doc struct {
		ModelPicker modelPicker `json:"modelPicker"`
	}
	if err := json.Unmarshal(overlayFile(p).Data, &doc); err != nil {
		t.Fatalf("overlay %s: %v", overlayFile(p).Data, err)
	}
	return doc.ModelPicker.Options
}

func checkPickerDescriptions(t *testing.T, session string, p agent.Plan, rows []pickerRow) {
	t.Helper()
	for _, r := range rows {
		if !strings.Contains(r.Description, " · proxy 127.0.0.1:") || len(r.Description) > 80 {
			t.Errorf("row description %q", r.Description)
		}
	}
	if got := p.Set["ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION"]; session == "dev" && !strings.HasPrefix(got, "Databricks · dev · proxy ") {
		t.Errorf("opus description %q", got)
	}
}

// A Databricks session lists ultra and the other signed-in workspaces; a login session stays direct with no rows.
func TestClaudePickerRows(t *testing.T) {
	for _, c := range []struct {
		session string
		peers   []string // profile prefixes in row order
	}{{"ultra", nil}, {"dev", []string{"ultra", "qa"}}} {
		t.Run(c.session, func(t *testing.T) {
			hermetic(t, pickerFixture)
			var scope []string
			stubMint(t, func(_ context.Context, profiles []string, _ int) (string, error) {
				scope = profiles
				return "cct_P", nil
			})
			p, err := claudePlan(t.Context(), agent.LaunchOptions{Provider: c.session}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			var rows []pickerRow
			if len(p.Files) > 0 {
				rows = pickerModels(t, p)
			}
			for _, r := range rows {
				prefix, _, _ := strings.Cut(r.Model, ":")
				if r.BehavesAs == "" || (prefix != "ultra" && strings.Contains(r.Model, "fable")) {
					t.Errorf("bad row %+v", r)
				}
				if len(order) == 0 || order[len(order)-1] != prefix {
					order = append(order, prefix)
				}
			}
			checkPickerDescriptions(t, c.session, p, rows)
			if c.session == "ultra" && (p.Set["ANTHROPIC_BASE_URL"] != "" || hasKeyHelper(p)) {
				t.Errorf("login session proxied: base %q keyHelper %v", p.Set["ANTHROPIC_BASE_URL"], hasKeyHelper(p))
			}
			if !slices.Equal(order, c.peers) {
				t.Errorf("row profiles %v, want %v", order, c.peers)
			}
			for _, n := range c.peers {
				if !slices.Contains(scope, n) {
					t.Errorf("scope %v lacks %s", scope, n)
				}
			}
			if slices.Contains(scope, "prod") {
				t.Errorf("unsigned prod in scope %v", scope)
			}
		})
	}
}

// A headless launch carries no picker.
func TestClaudePickerSkipsHeadless(t *testing.T) {
	hermetic(t, pickerFixture)
	p, err := claudePlan(t.Context(), agent.LaunchOptions{Provider: "dev"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(overlayFile(p).Data), "modelPicker") {
		t.Errorf("headless overlay carries a picker: %s", overlayFile(p).Data)
	}
}

func TestClaudeArgvFlagsAreDeclared(t *testing.T) {
	hermetic(t, gwProviders)
	t.Setenv("TEST_GW_KEY", fakeToken)
	budget := 1.0
	req := agent.RunOptions{BudgetUSD: &budget, NoPersist: true, SafeMode: true, DebugFile: "log",
		SystemPrompt: "system", AppendSystemPrompt: "append", OutputSchema: "{}", SessionID: "s1",
		Persona: &agent.Schema{Name: "fixture", Prompt: "prompt"}}
	for _, live := range []bool{false, true} {
		for _, tools := range []bool{false, true} {
			req.Tools, req.NoTools = tools, !tools
			argv := append(claudeStreamArgs(req, live), toolArgs(req)...)
			st := agent.Settings{Headless: true, Tools: tools, Effort: "high",
				Permissions: agent.Permissions{Allow: []agent.Rule{{Tool: "Read"}}, Deny: []agent.Rule{{Tool: "Edit"}}}}
			_, argv, _, cleanup, err := claudeAgent.RenderSettings(t.Context(), st, nil, argv)
			cleanup()
			if err != nil {
				t.Fatal(err)
			}
			if got := row.CLI.Undeclared(argv); len(got) > 0 {
				t.Errorf("live=%v tools=%v: undeclared %v", live, tools, got)
			}
		}
	}
	p, err := claudePlan(t.Context(), agent.LaunchOptions{Provider: "gw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.CLI.Undeclared(p.Argv); len(got) > 0 {
		t.Errorf("interactive: undeclared %v", got)
	}
}

// hasKeyHelper reports an apiKeyHelper in the plan's --settings layer (the autoMode layer alone is not one).
func hasKeyHelper(p agent.Plan) bool {
	return strings.Contains(string(overlayFile(p).Data), "apiKeyHelper") ||
		slices.ContainsFunc(p.Argv, func(a string) bool { return strings.Contains(a, "apiKeyHelper") })
}
