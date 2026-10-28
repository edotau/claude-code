package vendors

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"strconv"
)

const secret = "sk-SECRET-123"

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir()) // isolates GeminiLoginPresent's AgyLoginMarker lookup from the real machine
	writeGatewayOverlay(t)
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, secret)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get(provider)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Target{Provider: p, Model: model}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestConfigure(t *testing.T) {
	ctx := WithDirect(context.Background()) // these rows pin the direct hand-off; routing is covered in router_route_test.go
	cases := []struct {
		vendor, provider, model string
		h                       *Headless
		wantArgv                []string // substrings, in order
		wantEnv                 map[string]string
		wantErr                 string
	}{
		{vendor: "codex", provider: "openai", model: "gpt-5",
			wantArgv: []string{`model_provider="harness-openai"`, `model="gpt-5"`, `base_url = "https://api.openai.com/v1"`, `wire_api = "responses"`, `env_key = "HARNESS_CODEX_API_KEY"`},
			wantEnv:  map[string]string{CodexKeyEnv: secret}},
		{vendor: "codex", provider: "openai", model: "gpt-5", h: &Headless{Prompt: "-hi", Effort: "high", SessionID: "s1"},
			wantArgv: []string{"exec", `model_reasoning_effort="high"`, "--skip-git-repo-check", "--sandbox", "read-only", "resume", "s1", "-"}},
		{vendor: "codex", provider: "gateway", model: "x", wantErr: "no responses route"},
		// gemini runs only on agy's own Google login (no GEMINI_API_KEY fallback); target() isolates
		// HOME with no agy login marker, so both headless and interactive configure must refuse.
		// Success-with-login-marker is covered by TestGeminiLoginPrecedence.
		{vendor: "gemini", provider: "antigravity", model: "gemini-2.5-pro", h: &Headless{Prompt: "hi"},
			wantErr: "agy is not signed in"},
		{vendor: "gemini", provider: "antigravity", model: "gemini-2.5-pro",
			wantErr: "agy is not signed in"},
		{vendor: "gemini", provider: "openai", model: "gpt-5", wantErr: "runs only on its Google sign-in"},
		{vendor: "opencode", provider: "openai", model: "gpt-5", h: &Headless{Prompt: "hi", SessionID: "ses", Effort: "max", Tools: true},
			wantArgv: []string{"run", "-m", "harness-openai/gpt-5", "--session", "ses", "--variant", "max", "hi"},
			wantEnv:  map[string]string{OpenCodeKeyEnv: secret}},
		// --auto and each -f ride before the "--" that stops -f from swallowing the prompt as another path.
		{vendor: "opencode", provider: "openai", model: "gpt-5",
			h:        &Headless{Prompt: "hi", Autonomous: true, Attach: []string{"/tmp/a.txt", "/tmp/b.txt"}},
			wantArgv: []string{"run", "-m", "harness-openai/gpt-5", "--auto", "-f", "/tmp/a.txt", "-f", "/tmp/b.txt", "--", "hi"},
			wantEnv:  map[string]string{OpenCodeKeyEnv: secret}},
		{vendor: "copilot", provider: "anthropic", model: "claude-opus-5",
			wantEnv: map[string]string{"COPILOT_PROVIDER_TYPE": "anthropic", "COPILOT_PROVIDER_API_KEY": secret, "COPILOT_PROVIDER_BEARER_TOKEN": "", "COPILOT_MODEL": "claude-opus-5"}},
		{vendor: "copilot", provider: "openai", model: "gpt-5",
			wantEnv: map[string]string{"COPILOT_PROVIDER_TYPE": "openai", "COPILOT_PROVIDER_BEARER_TOKEN": secret, "COPILOT_PROVIDER_BASE_URL": "https://api.openai.com/v1"}},
		{vendor: "opencode", provider: "max", model: "claude-opus-5", wantErr: "cli (subscription)"},
		{vendor: "nope", provider: "openai", model: "gpt-5", wantErr: "unknown agent"},
	}
	for _, c := range cases {
		t.Run(c.vendor+"/"+c.provider, func(t *testing.T) {
			tg := target(t, c.provider, c.model)
			var env, argv []string
			var err error
			if c.h != nil {
				env, argv, err = ConfigureHeadless(ctx, c.vendor, tg, *c.h)
			} else {
				env, argv, err = Configure(ctx, c.vendor, tg, "")
			}
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("env:  %q\nargv: %q", env, argv)
			joined := strings.Join(argv, "\x00")
			if strings.Contains(joined, secret) {
				t.Fatalf("secret leaked into argv: %q", argv)
			}
			pos := 0
			for _, want := range c.wantArgv {
				i := strings.Index(joined[pos:], want)
				if i < 0 {
					t.Fatalf("argv %q missing %q (in order)", argv, want)
				}
				pos += i + len(want)
			}
			got := envMap(env)
			for k, v := range c.wantEnv {
				if got[k] != v {
					t.Errorf("env %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestCodexChatGPTLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no bin/codex: the auth.json fallback is what this test exercises
	t.Setenv("PATH", t.TempDir())
	tg := target(t, "chatgpt", "gpt-5.5")
	if _, _, err := Configure(context.Background(), "codex", tg, ""); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("no auth.json: err = %v, want a codex login hint", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, argv, err := ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-c", `model_provider="openai"`, "-c", `model="gpt-5.5"`, "--skip-git-repo-check", "--sandbox", "read-only", "-"}
	if len(env) != 0 || !slices.Equal(argv, want) {
		t.Errorf("env %q argv %q, want no env and %q", env, argv, want)
	}
	if _, _, err := Configure(context.Background(), "gemini", tg, ""); err == nil || !strings.Contains(err.Error(), "codex sign-in") {
		t.Errorf("only codex may use the chatgpt login: %v", err)
	}
}

func TestCopilotLogin(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	env, argv, err := ConfigureHeadless(context.Background(), "copilot", tg, Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	for k, v := range map[string]string{"COPILOT_MODEL": "claude-opus-5.5", "COPILOT_PROVIDER_BASE_URL": "", "COPILOT_OFFLINE": ""} {
		if w, ok := got[k]; !ok || w != v {
			t.Errorf("env %s = %q (set %v), want %q", k, w, ok, v)
		}
	}
	if want := []string{"--prompt", "hi", "--allow-all-tools"}; !slices.Equal(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if _, _, err := Configure(context.Background(), "codex", tg, ""); err == nil || !strings.Contains(err.Error(), "copilot sign-in") {
		t.Errorf("codex must refuse the copilot login: %v", err)
	}
}

// copilotPinArgv isolation: no gh/token env so CopilotLaunchToken (called unconditionally inside
// copilotLogin) resolves to "" instead of touching a real GitHub session.
func noCopilotLogin(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
}

// Interactive copilot (h == nil, own GitHub-login route): the configure page's pins ride argv as
// --reasoning-effort/--context when set, and neither flag appears when both pins are unset.
func TestCopilotLoginArgvCarriesPinsWhenSetOmitsWhenUnset(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	noCopilotLogin(t)
	for _, k := range []string{providers.PinCopilotEffort, providers.PinCopilotContext} {
		t.Setenv(k, "")
	}

	_, argv, err := Configure(context.Background(), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--reasoning-effort") || slices.Contains(argv, "--context") {
		t.Errorf("argv %q with no pins set, want neither flag", argv)
	}

	setPin(t, providers.PinCopilotEffort, "xhigh")
	setPin(t, providers.PinCopilotContext, "long_context")
	_, argv, err = Configure(context.Background(), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--reasoning-effort", "xhigh", "--context", "long_context", "--allow-all"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// codexHeadless carries CODEX_REASONING_EFFORT into -c model_reasoning_effort when the call names no
// explicit Effort; an explicit Headless.Effort still wins over the pin (TestConfigure's codex/Effort
// case already proves the no-pin explicit path, so this proves the pin-only and pin-vs-explicit paths).
func TestCodexHeadlessEffortPinUsedWhenEffortEmptyExplicitWinsOverIt(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	setPin(t, providers.PinCodexEffort, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}

	_, argv, err := ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "model_reasoning_effort=\"\"") || slices.ContainsFunc(argv, func(s string) bool { return strings.Contains(s, "model_reasoning_effort=") }) {
		t.Errorf("argv %q with no pin and no explicit effort, want no model_reasoning_effort flag", argv)
	}

	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	_, argv, err = ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the CODEX_REASONING_EFFORT pin (xhigh) carried in", argv)
	}

	// An explicit Headless.Effort still wins over the pin.
	_, argv, err = ConfigureHeadless(context.Background(), "codex", tg, Headless{Prompt: "hi", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `model_reasoning_effort="low"`) || slices.Contains(argv, `model_reasoning_effort="xhigh"`) {
		t.Errorf("argv = %q, want the explicit effort (low) to beat the xhigh pin", argv)
	}
}

// Interactive copilot (h == nil, BYOK/gateway route): same pin-to-argv wiring as the own-login route.
func TestCopilotBYOKArgvCarriesPinsWhenSet(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	setPin(t, providers.PinCopilotEffort, "low")
	setPin(t, providers.PinCopilotContext, "default")

	_, argv, err := Configure(context.Background(), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--reasoning-effort", "low", "--context", "default", "--allow-all"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// ConfigureHeadless (h != nil) ignores the pins: copilot always runs headless through copilotRunner's
// own SDK options, never this argv path, so copilotHeadless must stay untouched by the pins.
func TestCopilotHeadlessArgvIgnoresPins(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	noCopilotLogin(t)
	setPin(t, providers.PinCopilotEffort, "max")
	setPin(t, providers.PinCopilotContext, "long_context")

	_, argv, err := ConfigureHeadless(context.Background(), "copilot", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--prompt", "hi"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q (pins must not leak into the headless SDK path)", argv, want)
	}
}

// The registry's "auto" slot must not override copilot's own default model: COPILOT_MODEL goes out blank.
func TestCopilotLoginAutoDefersToCopilotDefault(t *testing.T) {
	env, _, err := Configure(context.Background(), "copilot", target(t, "personal", "auto"), "")
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := envMap(env)["COPILOT_MODEL"]; !ok || w != "" {
		t.Errorf("COPILOT_MODEL = %q (set %v), want blank", w, ok)
	}
}

// TestLaunchSpecAppliedOnEveryRoute: applyLaunch runs after every interactive route Configure can
// reach (own/github-copilot login, a keyed gateway provider), so no route can land without the
// vendor's spec env/argv — the failure this guards is a route added later that forgets to call it.
func TestLaunchSpecAppliedOnEveryRoute(t *testing.T) {
	cases := []struct {
		name             string
		vendor           string
		provider, model  string
		setup            func(t *testing.T) // extra fixture beyond target(), e.g. a login marker
		wantEnvContains  string
		wantArgvContains string
	}{
		{
			name: "opencode/gateway (keyed provider)", vendor: "opencode", provider: "gateway", model: "anthropic/claude-opus-5",
			wantEnvContains: "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1",
		},
		{
			name: "copilot/gateway (keyed provider)", vendor: "copilot", provider: "gateway", model: "anthropic/claude-opus-5",
			wantArgvContains: "--allow-all",
		},
		{
			name: "copilot/github-copilot (own login)", vendor: "copilot", provider: "personal", model: "claude-opus-5.5",
			wantArgvContains: "--allow-all",
		},
		{
			name: "gemini/own Google login", vendor: "gemini", provider: "antigravity", model: "gemini-2.5-pro",
			setup: func(t *testing.T) {
				home := os.Getenv("HOME")
				marker := AgyLoginMarker(home)
				if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantArgvContains: "--dangerously-skip-permissions",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tg := target(t, c.provider, c.model)
			if c.setup != nil {
				c.setup(t)
			}
			env, argv, err := Configure(context.Background(), c.vendor, tg, "")
			if err != nil {
				t.Fatal(err)
			}
			if c.wantEnvContains != "" && !slices.Contains(env, c.wantEnvContains) {
				t.Errorf("env = %q, want to contain %q", env, c.wantEnvContains)
			}
			if c.wantArgvContains != "" && !slices.Contains(argv, c.wantArgvContains) {
				t.Errorf("argv = %q, want to contain %q", argv, c.wantArgvContains)
			}
		})
	}

	// opencode's own GitHub Copilot login (ErrOwnLogin on the interactive leg bypasses Configure
	// entirely) still carries the spec through the exported LaunchSpec escape hatch.
	if env, _ := LaunchSpec("opencode"); !slices.Contains(env, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1") {
		t.Errorf("LaunchSpec(opencode) env = %q, want to contain OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1", env)
	}
}

// TestHeadlessLegsGetNoLaunchArgs: a tools-off headless leg must never pick up a vendor's allow-all /
// skip-permissions launch arg — that would silently re-enable tools the caller explicitly turned off.
func TestHeadlessLegsGetNoLaunchArgs(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	_, argv, err := ConfigureHeadless(context.Background(), "copilot", tg, Headless{Prompt: "hi", Tools: false})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--allow-all") {
		t.Errorf("copilot headless tools-off argv = %q, must not contain --allow-all", argv)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	gtg := target(t, "antigravity", "gemini-2.5-pro")
	t.Setenv("HOME", home) // target() reset HOME to its own tempdir; point it back at the marker
	_, argv, err = ConfigureHeadless(context.Background(), "gemini", gtg, Headless{Prompt: "hi", Tools: false})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--dangerously-skip-permissions") {
		t.Errorf("gemini headless tools-off argv = %q, must not contain --dangerously-skip-permissions", argv)
	}
}

func TestOpenCodeConfigShape(t *testing.T) {
	tg := target(t, "gateway", "anthropic/claude-opus-5")
	env, argv, err := ConfigureHeadless(WithDirect(context.Background()), "opencode", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model      string                    `json:"model"`
		Permission any                       `json:"permission"`
		Provider   map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	prov := cfg.Provider["harness-gateway"]
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/anthropic" || opts["baseURL"] != "https://gateway.example.com/api/v1" {
		t.Errorf("provider block = %v", prov)
	}
	if opts["apiKey"] != "{env:"+OpenCodeKeyEnv+"}" || opts["headers"].(map[string]any)["Authorization"] != "Bearer {env:"+OpenCodeKeyEnv+"}" {
		t.Errorf("auth must reference the env var, got %v", opts)
	}
	if cfg.Permission != "deny" || cfg.Model != "harness-gateway/anthropic/claude-opus-5" {
		t.Errorf("tools-off permission/model wrong: %+v", cfg)
	}
	if strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], secret) || strings.Contains(strings.Join(argv, " "), secret) {
		t.Error("secret inlined into config or argv")
	}
}

// TestOpenCodeDirArgv covers both argv builders opencode's workdir plumbing goes through: the BYOK/gateway
// builder (opencode(), reached via ConfigureHeadless) and the hub ladder's own (OpencodeRunArgv). Both must
// add --dir only when Headless.WorkDir is set, and never otherwise.
func TestOpenCodeDirArgv(t *testing.T) {
	t.Run("BYOK builder", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		_, argv, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv, err = ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})

	t.Run("hub ladder", func(t *testing.T) {
		_, argv := OpencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi", WorkDir: "/work/dir"})
		if i := slices.Index(argv, "--dir"); i == -1 || argv[i+1] != "/work/dir" {
			t.Errorf("argv = %q, want --dir /work/dir", argv)
		}

		_, argv = OpencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		if slices.Contains(argv, "--dir") {
			t.Errorf("argv = %q, must not contain --dir when WorkDir is unset", argv)
		}
	})
}

// TestOpenCodeOverlayEnvOnEveryRoute: applyLaunch wires OPENCODE_CONFIG/OPENCODE_TUI_CONFIG into every
// opencode launch path — own login (LaunchSpec), the BYOK/gateway route (Configure) and the hub ladder's
// own builder (OpencodeRunArgv) — not just the applyLaunch unit the overlay's own test already covers.
func TestOpenCodeOverlayEnvOnEveryRoute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hasOverlayEnv := func(t *testing.T, env []string) {
		t.Helper()
		m := envMap(env)
		if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
			t.Errorf("env = %v, want both OPENCODE_CONFIG and OPENCODE_TUI_CONFIG set", env)
		}
	}

	t.Run("own login (LaunchSpec)", func(t *testing.T) {
		env, _ := LaunchSpec("opencode")
		hasOverlayEnv(t, env)
	})

	t.Run("BYOK/gateway (Configure)", func(t *testing.T) {
		tg := target(t, "gateway", "anthropic/claude-opus-5")
		env, _, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		hasOverlayEnv(t, env)
	})

	t.Run("hub ladder (OpencodeRunArgv)", func(t *testing.T) {
		env, _ := OpencodeRunArgv("github-copilot/claude-opus-5", Headless{Prompt: "hi"})
		hasOverlayEnv(t, env)
	})
}

// TestOpenCodeOwnLoginHeadlessToolsOffDeny: opencode's own-login headless leg (OwnLoginHeadless) still
// rides OPENCODE_CONFIG_CONTENT={"permission":"deny"} when tools are off, alongside the new overlay env.
func TestOpenCodeOwnLoginHeadlessToolsOffDeny(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, _, err := OwnLoginHeadless("opencode", Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	if m["OPENCODE_CONFIG_CONTENT"] != `{"permission":"deny"}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the tools-off deny", m["OPENCODE_CONFIG_CONTENT"])
	}
	if m["OPENCODE_CONFIG"] == "" || m["OPENCODE_TUI_CONFIG"] == "" {
		t.Errorf("env = %v, want both overlay paths alongside the deny", env)
	}

	env, _, err = OwnLoginHeadless("opencode", Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envMap(env)["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Errorf("tools=true must carry no OPENCODE_CONFIG_CONTENT deny: %v", env)
	}
}

// agy has no custom base URL, so a gateway provider for the gemini vendor must fail fast with a clear error.
func TestGeminiGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Kind: providers.DialectGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
	_, _, err := Configure(context.Background(), "gemini", providers.Target{Provider: p, Model: "m"}, "")
	if err == nil || !strings.Contains(err.Error(), "no custom base URL") {
		t.Fatalf("err = %v, want a clear \"no custom base URL\" error", err)
	}
}

// gemini runs only on agy's cached Google login (AgyLoginMarker under HOME): no marker refuses to
// configure (no GEMINI_API_KEY fallback), a marker on disk lets it through with no key injected.
func TestGeminiLoginPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if GeminiLoginPresent() {
		t.Fatal("no marker on disk must read as not logged in")
	}
	tg := target(t, "antigravity", "gemini-2.5-pro")
	// target() reset HOME to its own tempdir; point it back at ours for the marker check.
	t.Setenv("HOME", home)

	if _, _, err := ConfigureHeadless(context.Background(), "gemini", tg, Headless{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("without a login marker, err = %v, want a not-signed-in error", err)
	}

	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !GeminiLoginPresent() {
		t.Fatal("marker on disk must read as logged in")
	}

	env, _, err := ConfigureHeadless(context.Background(), "gemini", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := envMap(env)["GEMINI_API_KEY"]; ok {
		t.Errorf("with a login marker, GEMINI_API_KEY must not be set, got %q", got)
	}
}

// opencodeMenu unmarshals OPENCODE_CONFIG_CONTENT and returns just the model menu + the top-level model id.
func opencodeMenu(t *testing.T, env []string, id string) (map[string]any, string) {
	t.Helper()
	var cfg struct {
		Model    string                    `json:"model"`
		Provider map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	menu, _ := cfg.Provider[id]["models"].(map[string]any)
	return menu, cfg.Model
}

func TestOpenCodeModelMenu(t *testing.T) {
	t.Run("known tier id", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := Configure(context.Background(), "opencode", tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		want := map[string]bool{"claude-opus-5-5": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": true}
		if len(menu) != len(want) {
			t.Fatalf("menu = %v, want exactly the 4 tier ids", menu)
		}
		for id := range want {
			if _, ok := menu[id]; !ok {
				t.Errorf("menu missing tier id %q: %v", id, menu)
			}
		}
	})

	t.Run("model not in table", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-custom")
		env, _, err := Configure(context.Background(), "opencode", tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, model := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 5 {
			t.Errorf("menu = %v, want 5 keys (4 tiers + the requested model)", menu)
		}
		if model != "harness-anthropic/claude-custom" {
			t.Errorf("model = %q, want harness-anthropic/claude-custom", model)
		}
	})

	t.Run("headless carries no menu", func(t *testing.T) {
		tg := target(t, "anthropic", "claude-sonnet-5-5")
		env, _, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		if len(menu) != 1 {
			t.Errorf("headless menu = %v, want exactly 1 key", menu)
		}
	})
}

func TestRedactConfig(t *testing.T) {
	in := map[string]any{
		"mcp": map[string]any{
			"x": map[string]any{"environment": map[string]any{"K": "secretval"}},
			"y": map[string]any{"headers": map[string]any{"Authorization": "Bearer secretval"}},
		},
		"provider": map[string]any{"harness-anthropic": map[string]any{"options": map[string]any{"apiKey": "{env:HARNESS_OPENCODE_API_KEY}"}}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := RedactConfig(string(b))
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	mcp := out["mcp"].(map[string]any)
	if mcp["x"].(map[string]any)["environment"].(map[string]any)["K"] != "<redacted>" {
		t.Errorf("mcp.x.environment not redacted: %v", mcp["x"])
	}
	if mcp["y"].(map[string]any)["headers"].(map[string]any)["Authorization"] != "<redacted>" {
		t.Errorf("mcp.y.headers not redacted: %v", mcp["y"])
	}
	provider := out["provider"].(map[string]any)
	apiKey := provider["harness-anthropic"].(map[string]any)["options"].(map[string]any)["apiKey"]
	if apiKey != "{env:HARNESS_OPENCODE_API_KEY}" {
		t.Errorf("provider.options.apiKey must not be touched, got %v", apiKey)
	}

	if got := RedactConfig("{not json"); got != "{not json" {
		t.Errorf("malformed input must be returned unchanged, got %q", got)
	}
}

// TestRedactConfigProviderOptionsHeaders: provider.<name>.options.headers values must be blanked even
// with no "mcp" section in the config at all (opencode's headless/BYOK configs carry no mcp key).
func TestRedactConfigProviderOptionsHeaders(t *testing.T) {
	in := map[string]any{"provider": map[string]any{
		"x": map[string]any{"options": map[string]any{"headers": map[string]any{"h": "sk-abc123"}}},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := RedactConfig(string(b))
	if strings.Contains(got, "sk-abc123") {
		t.Errorf("secret leaked into redacted config: %s", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["mcp"]; ok {
		t.Errorf("no mcp key was present in the input; output must not invent one: %v", out)
	}
	headers := out["provider"].(map[string]any)["x"].(map[string]any)["options"].(map[string]any)["headers"].(map[string]any)
	if headers["h"] != "<redacted>" {
		t.Errorf("provider.x.options.headers.h = %v, want <redacted>", headers["h"])
	}
}

// TestRedactConfigNonSecretFields: model ids and baseURL pass through untouched.
func TestRedactConfigNonSecretFields(t *testing.T) {
	in := map[string]any{
		"model": "harness-anthropic/claude-opus-5",
		"provider": map[string]any{
			"harness-anthropic": map[string]any{
				"options": map[string]any{"baseURL": "https://api.anthropic.com/v1"},
			},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := RedactConfig(string(b))
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "harness-anthropic/claude-opus-5" {
		t.Errorf("model = %v, must survive unchanged", out["model"])
	}
	opts := out["provider"].(map[string]any)["harness-anthropic"].(map[string]any)["options"].(map[string]any)
	if opts["baseURL"] != "https://api.anthropic.com/v1" {
		t.Errorf("baseURL = %v, must survive unchanged", opts["baseURL"])
	}
}

// RedactConfig must blank a secret-naming .command flag's value (bare or "=" form) and a .url's whole query
// string, while leaving non-secret flags, the bare command name, and the flag names themselves intact.
func TestRedactConfigCommandAndURL(t *testing.T) {
	in := map[string]any{"mcp": map[string]any{
		"x": map[string]any{"command": []any{"srv", "--api-key", "SECRET1", "--token=SECRET2", "--verbose"}},
		"y": map[string]any{"url": "https://h/p?token=SECRET3"},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := RedactConfig(string(b))
	for _, secret := range []string{"SECRET1", "SECRET2", "SECRET3"} {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaked into redacted config: %s", secret, got)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	mcp := out["mcp"].(map[string]any)
	cmd := mcp["x"].(map[string]any)["command"].([]any)
	wantCmd := []any{"srv", "--api-key", "<redacted>", "--token=<redacted>", "--verbose"}
	if len(cmd) != len(wantCmd) {
		t.Fatalf("command = %v, want %v", cmd, wantCmd)
	}
	for i, want := range wantCmd {
		if cmd[i] != want {
			t.Errorf("command[%d] = %v, want %v", i, cmd[i], want)
		}
	}
	if url := mcp["y"].(map[string]any)["url"]; url != "https://h/p?<redacted>" {
		t.Errorf("url = %v, want https://h/p?<redacted>", url)
	}
}

func TestResolveTargetFallsBackWhenSessionProviderCannotServe(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no real codex: login detection falls back to the auth.json fixture
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("GH_TOKEN", "")
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}

	// codex: subscription can't serve it, no ChatGPT login present -> target unchanged, no note.
	target, note, err := ResolveTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("expected no fallback without a codex login, got %q", note)
	}
	if target.Provider != sub {
		t.Fatalf("expected unchanged target, got %s", target.Provider.Name)
	}

	// With a ChatGPT login present, codex falls back to the chatgpt provider.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "codex", "auth.json")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex", "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target, note, err = ResolveTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Name != "chatgpt" {
		t.Fatalf("expected fallback to chatgpt, got %v", target.Provider)
	}
	if !strings.Contains(note, "chatgpt") {
		t.Fatalf("expected a note naming chatgpt, got %q", note)
	}

	// The CODEX_MODEL pin beats both the chatgpt provider's own opus slot and codexOwnModel's
	// config.toml (unset here), once the ChatGPT login fallback applies.
	setPin(t, providers.PinCodexModel, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "gpt-6.1-sol"}); err != nil {
		t.Fatal(err)
	}
	target, _, err = ResolveTarget(reg, "codex", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gpt-6.1-sol" {
		t.Fatalf("CODEX_MODEL pin = gpt-6.1-sol, want it to win the fallback target's model, got %q", target.Model)
	}

	// An explicit provider choice is never overridden by a fallback.
	target, note, err = ResolveTarget(reg, "codex", sel, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || target.Provider != sub {
		t.Fatalf("explicit provider must not fall back, got target=%v note=%q", target.Provider, note)
	}
}

// The GEMINI_AGENT_MODEL pin beats the gemini provider's own registry opus slot once agy's Google
// login fallback applies (subscription can't serve gemini directly).
func TestResolveTargetGeminiModelPinBeatsRegistryDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}

	setPin(t, providers.PinGeminiModel, "")
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: ""}); err != nil {
		t.Fatal(err)
	}
	target, _, err := ResolveTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := reg.Providers["antigravity"].Models["opus"]
	if target.Model != wantDefault {
		t.Fatalf("unpinned: target.Model = %q, want the registry's gemini opus slot %q", target.Model, wantDefault)
	}

	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-2.5-pro"}); err != nil {
		t.Fatal(err)
	}
	target, _, err = ResolveTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "gemini-2.5-pro" {
		t.Fatalf("GEMINI_AGENT_MODEL pin = gemini-2.5-pro, want it to win the fallback target's model, got %q", target.Model)
	}
}

// tomlString must take the quoted value only: a trailing comment or literal quotes must not ride into the model id.
func TestTomlString(t *testing.T) {
	cases := map[string]string{
		`"gpt-6-astra"`:             "gpt-6-astra",
		`"gpt-6-astra" # preferred`: "gpt-6-astra",
		`'gpt-6-astra'`:             "gpt-6-astra",
		`gpt-6-astra # bare`:        "gpt-6-astra",
		`"unterminated`:             "unterminated",
	}
	for in, want := range cases {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %q, want %q", in, got, want)
		}
	}
}

// copilot's own token precedence: COPILOT_GITHUB_TOKEN, GH_TOKEN, GITHUB_TOKEN, then an authenticated gh.
func TestCopilotLoginPresentHonoursEveryTokenVar(t *testing.T) {
	t.Setenv("PATH", t.TempDir())              // no gh on PATH
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // CLAUDE_CONFIG_DIR isolated so nothing real leaks in
	t.Setenv("COPILOT_HOME", t.TempDir())      // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir())     // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if CopilotLoginPresent() {
		t.Fatal("no token and no gh must read as not logged in")
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "tok")
		if !CopilotLoginPresent() {
			t.Errorf("%s must count as a copilot login", k)
		}
		t.Setenv(k, "")
	}
}

// BYOK on copilot: headers ride COPILOT_PROVIDER_HEADERS, a bearer route sets only the bearer var, and the
// prompt budget leaves room for the output cap.
func TestCopilotBYOKHeadersBearerAndBudget(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	tg.Provider.Headers = map[string]string{"X-Tenant": "t1", "Authorization-Extra": "z"}
	env, _, err := Configure(WithDirect(context.Background()), "copilot", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	if got["COPILOT_PROVIDER_HEADERS"] != "Authorization-Extra: z\nX-Tenant: t1" {
		t.Errorf("headers = %q", got["COPILOT_PROVIDER_HEADERS"])
	}
	if got["COPILOT_PROVIDER_API_KEY"] != "" || got["COPILOT_PROVIDER_BEARER_TOKEN"] == "" {
		t.Errorf("bearer route must set only the bearer var: key=%q bearer=%q", got["COPILOT_PROVIDER_API_KEY"], got["COPILOT_PROVIDER_BEARER_TOKEN"])
	}
	if v, ok := got["COPILOT_PROVIDER_MAX_PROMPT_TOKENS"]; ok && v == strconv.Itoa(models.ContextWindow("gpt-5")) {
		t.Errorf("prompt budget must leave room for %d output tokens, got %s", copilotMaxOutputTokens, v)
	}
}

// ValidCopilotToken: every documented GitHub token prefix passes at length, a short or wrongly-prefixed
// or whitespace-mangled value does not.
func TestValidCopilotToken(t *testing.T) {
	cases := []struct {
		name string
		tok  string
		want bool
	}{
		{"gho_ prefix", "gh" + "o_abcdefghijklmnopqrstuvwxyz", true},
		{"ghp_ prefix", "gh" + "p_abcdefghijklmnopqrstuvwxyz", true},
		{"ghu_ prefix", "gh" + "u_abcdefghijklmnopqrstuvwxyz", true},
		{"github_pat_ prefix", "github_" + "pat_abcdefghijklmnop", true},
		{"too short with valid prefix", "ghp_short", false},
		{"wrong prefix", "sk-abcdefghijklmnopqrstuvwxyz", false},
		{"no prefix at all", "abcdefghijklmnopqrstuvwxyz1234", false},
		{"empty", "", false},
		{"leading whitespace defeats the prefix check", " ghp_abcdefghijklmnopqrstuvwxyz", false},
		{"trailing whitespace still counts toward length but prefix matches", "gh" + "p_abcdefghijklmnopqrstuvwxyz ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidCopilotToken(c.tok); got != c.want {
				t.Errorf("ValidCopilotToken(%q) = %v, want %v", c.tok, got, c.want)
			}
		})
	}
}

// copilotAuthEnv isolates config/home/gh/keyring so copilotAuthSource reads nothing real; the fake `gh` on
// PATH reports an active account for hosts.yml but is never `security`/`secret-tool`, and the vault key comes
// from HARNESS_VAULT_KEY with an in-memory keyring behind it.
func copilotAuthEnv(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Keyring(t)
	t.Setenv("COPILOT_HOME", t.TempDir())
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	hosts := "github.com:\n    user: alice\n    users:\n        alice:\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gh := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin) // gh present, but no `security`/`secret-tool`: keyring.Available() is false
}

// A stored COPILOT_GITHUB_TOKEN (vault) outranks gh's active account.
func TestCopilotAuthSourceStoredTokenOutranksActiveAccount(t *testing.T) {
	copilotAuthEnv(t)
	if _, _, ok := secrets.Get(CopilotTokenEnv); ok {
		t.Fatal("precondition: no stored token yet")
	}
	a, ok := copilotAuthSource()
	if !ok || a.Source != srcActive || a.Account != "alice" {
		t.Fatalf("with no stored token: got %+v, want gh's active account alice", a)
	}
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	if v, src, ok := secrets.Get(CopilotTokenEnv); !ok || src != "vault" || v != "stored-tok" {
		t.Fatalf("secrets.Get(%s) = (%q, %q, %v), want (stored-tok, vault, true)", CopilotTokenEnv, v, src, ok)
	}
	a, ok = copilotAuthSource()
	if !ok || a.Source != SrcStoredToken || a.token != "stored-tok" {
		t.Fatalf("with a stored token: got %+v, want Source=%s token=stored-tok", a, SrcStoredToken)
	}
}

// A pinned gh user (COPILOT_PROFILE's login) still outranks a stored token.
func TestCopilotAuthSourcePinOutranksStoredToken(t *testing.T) {
	copilotAuthEnv(t)
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	pinCopilotLogin(t, "alice")
	a, ok := copilotAuthSource()
	if !ok || a.Source != srcPinned || a.Account != "alice" {
		t.Fatalf("a pin must outrank a stored token: got %+v", a)
	}
}

// The same token present only as a process env var (never stored) must not take the stored-token path:
// it falls through to copilot's own env-precedence order instead.
func TestCopilotAuthSourceEnvOnlyTokenIsNotStored(t *testing.T) {
	copilotAuthEnv(t)
	t.Setenv(CopilotTokenEnv, "env-tok")
	if _, src, ok := secrets.Get(CopilotTokenEnv); !ok || src != "env" {
		t.Fatalf("secrets.Get(%s) = (_, %q, %v), want source env", CopilotTokenEnv, src, ok)
	}
	a, ok := copilotAuthSource()
	if !ok || a.Source == SrcStoredToken {
		t.Fatalf("an env-only token must not resolve as %s: got %+v", SrcStoredToken, a)
	}
	// gh's active account still outranks copilot's own env-var order (srcActive beats CopilotTokenEnv here).
	if a.Source != srcActive {
		t.Fatalf("got %+v, want gh's active account to win over an env-only token", a)
	}
}

func TestResolveTargetGeminiFallsBackToGoogleLogin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_API_KEY", "")
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
	target, note, err := ResolveTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Provider != "google" || !strings.Contains(note, "Google login") {
		t.Fatalf("expected the Google-login gemini route, got %v / %q", target.Provider, note)
	}
	env, _, err := ConfigureHeadless(context.Background(), "gemini", target, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatalf("a login with no key must still configure: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GEMINI_API_KEY=") {
			t.Fatalf("login run must not inject a key: %v", env)
		}
	}
}

// writeGatewayOverlay registers a test-only third-party gateway (no such provider is built in).
func writeGatewayOverlay(t *testing.T) {
	t.Helper()
	body := `{"providers":{"anthropic":{"gateway":{"kind":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"key","header":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", body)
}
