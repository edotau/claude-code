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
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vendorconfig"
	"strconv"
)

const secret = "sk-SECRET-123"

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir()) // isolates GeminiLoginPresent's AgyLoginMarker lookup from the real machine
	writeGatewayOverlay(t, dir)
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
	ctx := context.Background()
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
		{vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro", h: &Headless{Prompt: "hi"},
			wantErr: "agy is not signed in"},
		{vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro",
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
		{vendor: "opencode", provider: "subscription", model: "claude-opus-5", wantErr: "passthrough"},
		{vendor: "nope", provider: "openai", model: "gpt-5", wantErr: "unknown vendor"},
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
	t.Setenv("PATH", t.TempDir()) // no real codex: the auth.json fallback is what this test exercises
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
	tg := target(t, "github-copilot", "claude-opus-5.5")
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

// The registry's "auto" slot must not override copilot's own default model: COPILOT_MODEL goes out blank.
func TestCopilotLoginAutoDefersToCopilotDefault(t *testing.T) {
	env, _, err := Configure(context.Background(), "copilot", target(t, "github-copilot", "auto"), "")
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
			name: "copilot/github-copilot (own login)", vendor: "copilot", provider: "github-copilot", model: "claude-opus-5.5",
			wantArgvContains: "--allow-all",
		},
		{
			name: "gemini/own Google login", vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro",
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
	tg := target(t, "github-copilot", "claude-opus-5.5")
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
	gtg := target(t, "gemini", "gemini-2.5-pro")
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
	env, argv, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
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

// agy has no custom base URL, so a gateway provider for the gemini vendor must fail fast with a clear error.
func TestGeminiGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Kind: providers.DialectGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthBearer, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
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
	tg := target(t, "gemini", "gemini-2.5-pro")
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
		tg := target(t, "anthropic", "claude-sonnet-5")
		env, _, err := Configure(context.Background(), "opencode", tg, "")
		if err != nil {
			t.Fatal(err)
		}
		menu, _ := opencodeMenu(t, env, "harness-anthropic")
		want := map[string]bool{"claude-opus-5-5": true, "claude-sonnet-5": true, "claude-haiku-4-5": true, "claude-fable-5-1": true}
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
		tg := target(t, "anthropic", "claude-sonnet-5")
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

func TestPermissionBlock(t *testing.T) {
	cases := []struct {
		name             string
		allow, deny, ask []string
		want             map[string]any // nil means permissionBlock must return nil
	}{
		{
			name:  "mixed",
			allow: []string{"Bash(*)", "Bash(go test:*)", "Bash(make:*)", "Edit(*)", "WebFetch(*)", "Read(*)", "Agent", "mcp__databricks__*"},
			deny:  []string{"Bash(rm -rf:*)"},
			ask:   []string{"Bash(git push:*)", "Bash(go test:*)"},
			want: map[string]any{
				"bash": map[string]any{"go test *": "ask", "make *": "allow", "git push *": "ask", "rm -rf *": "deny"},
				"edit": "allow", "webfetch": "allow",
			},
		},
		{
			name:  "lone blanket allow",
			allow: []string{"Bash(*)"},
			want:  map[string]any{"bash": map[string]any{"*": "allow"}},
		},
		{
			name: "global deny beats ask",
			deny: []string{"Bash(*)"},
			ask:  []string{"Bash(*)"},
			want: map[string]any{"bash": map[string]any{"*": "deny"}},
		},
		{
			name: "exact command without :*",
			deny: []string{"Bash(rm -rf /)"},
			want: map[string]any{"bash": map[string]any{"rm -rf /": "deny"}},
		},
		{
			name:  "unknown rules only",
			allow: []string{"Read(*)", "mcp__x__*", "Agent"},
			want:  nil,
		},
		{
			name: "all empty",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := permissionBlock(c.allow, c.deny, c.ask)
			if c.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			gotBash, _ := got["bash"].(map[string]any)
			wantBash, _ := c.want["bash"].(map[string]any)
			if len(gotBash) != len(wantBash) {
				t.Fatalf("bash = %v, want %v", gotBash, wantBash)
			}
			for k, v := range wantBash {
				if gotBash[k] != v {
					t.Errorf("bash[%q] = %v, want %v", k, gotBash[k], v)
				}
			}
			for _, cat := range []string{"edit", "webfetch"} {
				if want, ok := c.want[cat]; ok && got[cat] != want {
					t.Errorf("%s = %v, want %v", cat, got[cat], want)
				}
			}
		})
	}
}

func TestMCPBlock(t *testing.T) {
	servers := map[string]any{
		"stdio-full": map[string]any{"command": "bash", "args": []any{"x.sh"}, "env": map[string]any{"K": "v"}},
		"stdio-bare": map[string]any{"command": "cmd"},
		"http":       map[string]any{"url": "https://h.example", "headers": map[string]any{"X-A": "b"}},
		"unknown":    map[string]any{"transport": "pigeon"},
		"junk":       "not-a-map",
	}
	got := mcpBlock(servers)

	full, ok := got["stdio-full"].(map[string]any)
	if !ok {
		t.Fatalf("stdio-full missing: %v", got)
	}
	if full["type"] != "local" {
		t.Errorf("stdio-full.type = %v, want local", full["type"])
	}
	if cmd, _ := full["command"].([]any); len(cmd) != 2 || cmd[0] != "bash" || cmd[1] != "x.sh" {
		t.Errorf("stdio-full.command = %v, want [bash x.sh]", full["command"])
	}
	env, _ := full["environment"].(map[string]any)
	if env["K"] != "v" {
		t.Errorf("stdio-full.environment = %v, want {K: v}", full["environment"])
	}
	if full["enabled"] != true {
		t.Errorf("stdio-full.enabled = %v, want true", full["enabled"])
	}

	bare, ok := got["stdio-bare"].(map[string]any)
	if !ok {
		t.Fatalf("stdio-bare missing: %v", got)
	}
	if _, has := bare["environment"]; has {
		t.Errorf("stdio-bare must carry no environment key: %v", bare)
	}

	remote, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("http missing: %v", got)
	}
	if remote["type"] != "remote" {
		t.Errorf("http.type = %v, want remote", remote["type"])
	}
	headers, _ := remote["headers"].(map[string]any)
	if headers["X-A"] != "b" {
		t.Errorf("http.headers = %v, want {X-A: b}", remote["headers"])
	}

	if _, ok := got["unknown"]; ok {
		t.Errorf("unknown transport must be absent: %v", got["unknown"])
	}
	if _, ok := got["junk"]; ok {
		t.Errorf("non-object value must be absent: %v", got["junk"])
	}
}

// TestFilterAllowedMCPDropsUnlisted: a server not named in the vendor's MCP.Allow spec is dropped and
// reported, mirroring the project-MCP-approval skip path (HARNESS_OPENCODE_MCP_SKIPPED).
func TestFilterAllowedMCPDropsUnlisted(t *testing.T) {
	servers := map[string]any{
		"shared": map[string]any{"command": "global"},
		"other":  map[string]any{"command": "nope"},
	}
	kept, dropped := filterAllowedMCP(servers, []string{"shared"})
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want exactly {shared: ...}", kept)
	}
	if _, ok := kept["shared"]; !ok {
		t.Errorf("kept missing shared: %v", kept)
	}
	if _, ok := kept["other"]; ok {
		t.Errorf("kept must not contain other: %v", kept)
	}
	if !slices.Contains(dropped, "other") {
		t.Errorf("dropped = %v, want to contain other", dropped)
	}
	if slices.Contains(dropped, "shared") {
		t.Errorf("dropped = %v, must not contain the allowed server shared", dropped)
	}

	// No overlay / empty allowlist => nothing is wired, every server reported dropped.
	kept, dropped = filterAllowedMCP(servers, nil)
	if len(kept) != 0 {
		t.Errorf("kept with empty allowlist = %v, want empty", kept)
	}
	if len(dropped) != len(servers) {
		t.Errorf("dropped with empty allowlist = %v, want all %d servers", dropped, len(servers))
	}
}

// TestGuardPermissionAppliesGuardRulesOnlyWhenOptedIn: a vendor not opted into UseGuardRules gets plain
// "allow"; opted-in carries the deny `rm -rf /` and ask `git reset --hard *` through permissionBlock.
func TestGuardPermissionAppliesGuardRulesOnlyWhenOptedIn(t *testing.T) {
	vc, err := vendorconfig.Parse(nil, nil) // embedded defaults: deny rm -rf /, ask git reset --hard:*
	if err != nil {
		t.Fatal(err)
	}

	plain := vendorconfig.Vendor{}
	if got := guardPermission(vc, plain); got != "allow" {
		t.Errorf("not opted in: guardPermission = %v, want \"allow\"", got)
	}

	guarded := vendorconfig.Vendor{Permission: vendorconfig.Permission{UseGuardRules: true}}
	got, ok := guardPermission(vc, guarded).(map[string]any)
	if !ok {
		t.Fatalf("opted in: guardPermission = %v (%T), want a map", got, got)
	}
	bash, _ := got["bash"].(map[string]any)
	if bash["rm -rf /"] != "deny" {
		t.Errorf("bash[rm -rf /] = %v, want deny", bash["rm -rf /"])
	}
	if bash["git reset --hard *"] != "ask" {
		t.Errorf("bash[git reset --hard *] = %v, want ask", bash["git reset --hard *"])
	}
	// template-only ask (not in the old hand-copied defaults.json guardRules) proves the guard
	// list now comes from settings.Render(), not a hand-maintained subset.
	if bash["git push --force origin main *"] != "ask" {
		t.Errorf("bash[git push --force origin main *] = %v, want ask", bash["git push --force origin main *"])
	}
}

func TestJSONField(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got := jsonField(filepath.Join(dir, "missing.json"), "field"); got != nil {
		t.Errorf("missing file = %v, want nil", got)
	}
	if got := jsonField(write("malformed.json", "{not json"), "field"); got != nil {
		t.Errorf("malformed json = %v, want nil", got)
	}
	if got := jsonField(write("absent.json", `{"other": {"a": 1}}`), "field"); got != nil {
		t.Errorf("absent field = %v, want nil", got)
	}
	if got := jsonField(write("nonobject.json", `{"field": "a string"}`), "field"); got != nil {
		t.Errorf("non-object field = %v, want nil", got)
	}
	got := jsonField(write("happy.json", `{"field": {"a": 1}}`), "field")
	if a, ok := got["a"].(float64); !ok || a != 1 {
		t.Errorf("happy path = %v, want {a: 1}", got)
	}
}

// writeOverlayFile drops body at vendorconfig's UserFile() (env.d/vendors.json), honoring whatever
// CLAUDE_CONFIG_DIR the calling test already set via target().
func writeOverlayFile(t *testing.T, body string) {
	t.Helper()
	p := vendorconfig.UserFile()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCodeInteractiveImports: Claude's project/global permissions are no longer imported wholesale
// (guardPermission owns opencode's permission block instead); MCP servers Claude approves are still
// read, but then gated by the vendorconfig MCP.Allow spec — no overlay wires nothing in, and an
// overlay allowing one name wires only that one, every other name landing on the skipped channel.
func TestOpenCodeInteractiveImports(t *testing.T) {
	tg := target(t, "anthropic", "claude-sonnet-5")

	proj := t.TempDir()
	t.Chdir(proj)
	cwd, err := os.Getwd() // t.TempDir() may be under a /var -> /private/var symlink on macOS
	if err != nil {
		t.Fatal(err)
	}

	writeJSON(t, filepath.Join(paths.ConfigDir(), ".claude.json"), map[string]any{
		"mcpServers": map[string]any{
			"shared": map[string]any{"command": "global"},
			"g":      map[string]any{"url": "https://g"},
		},
		"projects": map[string]any{
			cwd: map[string]any{"enabledMcpjsonServers": []string{"shared"}},
		},
	})
	writeJSON(t, filepath.Join(proj, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"shared":     map[string]any{"command": "project"},
		"unapproved": map[string]any{"command": "nope"},
	}})

	cfgOf := func(t *testing.T) (permission any, mcp map[string]struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
	}, skipped string) {
		t.Helper()
		env, _, err := Configure(context.Background(), "opencode", tg, "")
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Permission any `json:"permission"`
			MCP        map[string]struct {
				Type    string   `json:"type"`
				Command []string `json:"command"`
			} `json:"mcp"`
		}
		if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Permission, cfg.MCP, envMap(env)["HARNESS_OPENCODE_MCP_SKIPPED"]
	}

	// No env.d/vendors.json overlay: opencode's default MCP.Allow is empty, so Claude's approved
	// servers ("shared", "g") are read but every one of them is wired in nowhere, not even partially.
	// guardPermission, not the Claude-settings import, owns the permission block on this same (no
	// overlay, built-in defaults) call: the default vendor spec opts into UseGuardRules, so the
	// harness's own guard deny/ask rules surface.
	permission, mcp, skipped := cfgOf(t)
	if len(mcp) != 0 {
		t.Errorf("no overlay: mcp = %v, want empty (nothing on the default allowlist)", mcp)
	}
	for _, name := range []string{"shared", "g"} {
		if !strings.Contains(skipped, name) {
			t.Errorf("no overlay: HARNESS_OPENCODE_MCP_SKIPPED = %q, want it to contain %q", skipped, name)
		}
	}
	perm, ok := permission.(map[string]any)
	if !ok {
		t.Fatalf("permission = %v (%T), want a guard-rule map", permission, permission)
	}
	bash, _ := perm["bash"].(map[string]any)
	if bash["rm -rf /"] != "deny" {
		t.Errorf("permission.bash[rm -rf /] = %v, want deny", bash["rm -rf /"])
	}
	if bash["git reset --hard *"] != "ask" {
		t.Errorf("permission.bash[git reset --hard *] = %v, want ask", bash["git reset --hard *"])
	}
	// template-only ask, absent from the old hand-copied defaults.json guardRules: proves the
	// guard list rendered here comes from settings.Render(), not a stale local copy.
	if bash["git push --force origin main *"] != "ask" {
		t.Errorf("permission.bash[git push --force origin main *] = %v, want ask", bash["git push --force origin main *"])
	}
	if perm["edit"] != "allow" {
		t.Errorf("permission.edit = %v, want allow", perm["edit"])
	}

	// An overlay allowing only "shared" wires in just that one server (project wins over global), and
	// still drops "g" and the project-unapproved "unapproved" onto the skipped channel.
	writeOverlayFile(t, `{"vendors":{"opencode":{"mcp":{"allow":["shared"]}}}}`)
	_, mcp, skipped = cfgOf(t)
	if len(mcp) != 1 {
		t.Fatalf("overlay allows [shared]: mcp = %v, want exactly {shared: ...}", mcp)
	}
	if got := mcp["shared"].Command; len(got) != 1 || got[0] != "project" {
		t.Errorf("mcp.shared.command = %v, want [project] (project wins)", got)
	}
	if _, ok := mcp["g"]; ok {
		t.Errorf("mcp.g must be absent (not on the overlay's allowlist): %v", mcp["g"])
	}
	for _, name := range []string{"g", "unapproved"} {
		if !strings.Contains(skipped, name) {
			t.Errorf("HARNESS_OPENCODE_MCP_SKIPPED = %q, want it to contain %q", skipped, name)
		}
	}
	if strings.Contains(skipped, "shared") {
		t.Errorf("HARNESS_OPENCODE_MCP_SKIPPED = %q, must not contain the allowed server shared", skipped)
	}

	// A malformed .mcp.json must not fail the launch.
	if err := os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Configure(context.Background(), "opencode", tg, ""); err != nil {
		t.Fatalf("malformed .mcp.json must not fail the launch: %v", err)
	}

	headlessEnv, _, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var headlessCfg map[string]any
	if err := json.Unmarshal([]byte(envMap(headlessEnv)["OPENCODE_CONFIG_CONTENT"]), &headlessCfg); err != nil {
		t.Fatal(err)
	}
	if headlessCfg["permission"] != "deny" {
		t.Errorf("headless permission = %v, want deny", headlessCfg["permission"])
	}
	if _, ok := headlessCfg["mcp"]; ok {
		t.Errorf("headless must carry no mcp key: %v", headlessCfg["mcp"])
	}

	toolsEnv, _, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	var toolsCfg map[string]any
	if err := json.Unmarshal([]byte(envMap(toolsEnv)["OPENCODE_CONFIG_CONTENT"]), &toolsCfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := toolsCfg["permission"]; ok {
		t.Errorf("headless tools=true must carry no permission key: %v", toolsCfg["permission"])
	}
	if _, ok := toolsCfg["mcp"]; ok {
		t.Errorf("headless tools=true must carry no mcp key: %v", toolsCfg["mcp"])
	}
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

	if got := RedactConfig("{not json"); got != "{not json" {
		t.Errorf("malformed input must be returned unchanged, got %q", got)
	}
}

// TestProjectMCPApprovalGate pins projectMCPApproval's four outcomes for a project .mcp.json server, plus
// the invariant that a global ~/.claude.json mcpServers entry is imported regardless of project approval.
func TestProjectMCPApprovalGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	proj := t.TempDir()
	t.Chdir(proj)
	cwd, err := os.Getwd() // t.TempDir() may be under a /var -> /private/var symlink on macOS
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(proj, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"srv": map[string]any{"command": "proj-srv"},
	}})

	cases := []struct {
		name        string
		doc         map[string]any
		wantImport  bool
		wantSkipped bool
	}{
		{
			name:        "no projects entry",
			doc:         map[string]any{"mcpServers": map[string]any{"g": map[string]any{"command": "global"}}},
			wantImport:  false,
			wantSkipped: true,
		},
		{
			name: "enabledMcpjsonServers lists it",
			doc: map[string]any{
				"mcpServers": map[string]any{"g": map[string]any{"command": "global"}},
				"projects":   map[string]any{cwd: map[string]any{"enabledMcpjsonServers": []string{"srv"}}},
			},
			wantImport:  true,
			wantSkipped: false,
		},
		{
			name: "enableAllProjectMcpServers true",
			doc: map[string]any{
				"mcpServers":                 map[string]any{"g": map[string]any{"command": "global"}},
				"enableAllProjectMcpServers": true,
			},
			wantImport:  true,
			wantSkipped: false,
		},
		{
			name: "listed in both enabled and disabled",
			doc: map[string]any{
				"mcpServers": map[string]any{"g": map[string]any{"command": "global"}},
				"projects": map[string]any{cwd: map[string]any{
					"enabledMcpjsonServers":  []string{"srv"},
					"disabledMcpjsonServers": []string{"srv"},
				}},
			},
			wantImport:  false,
			wantSkipped: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeJSON(t, paths.ClaudeJSON(), c.doc)
			imp := readClaudeImport(cwd)
			if _, imported := imp.mcp["srv"]; imported != c.wantImport {
				t.Errorf("srv imported = %v, want %v", imported, c.wantImport)
			}
			if skipped := slices.Contains(imp.skippedMCP, "srv"); skipped != c.wantSkipped {
				t.Errorf("srv skipped = %v, want %v (skippedMCP=%v)", skipped, c.wantSkipped, imp.skippedMCP)
			}
			if _, ok := imp.mcp["g"]; !ok {
				t.Errorf("global ~/.claude.json mcpServers entry g must always be imported")
			}
		})
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
	sub, err := reg.Get("subscription")
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

	// An explicit provider choice is never overridden by a fallback.
	target, note, err = ResolveTarget(reg, "codex", sel, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || target.Provider != sub {
		t.Fatalf("explicit provider must not fall back, got target=%v note=%q", target.Provider, note)
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
	if copilotLoginPresent() {
		t.Fatal("no token and no gh must read as not logged in")
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "tok")
		if !copilotLoginPresent() {
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
	env, _, err := Configure(context.Background(), "copilot", tg, "")
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

// copilotAuthEnv isolates config/home/gh/keyring so copilotAuthSource reads nothing real; the fake `gh` on
// PATH reports an active account for hosts.yml but is never `security`/`secret-tool`, so Store/Get use the
// file plane only.
func copilotAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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

// A stored COPILOT_GITHUB_TOKEN (file plane) outranks gh's active account.
func TestCopilotAuthSourceStoredTokenOutranksActiveAccount(t *testing.T) {
	copilotAuthEnv(t)
	if _, _, ok := secrets.Get(CopilotTokenEnv); ok {
		t.Fatal("precondition: no stored token yet")
	}
	a, ok := copilotAuthSource()
	if !ok || a.Source != srcActive || a.Account != "alice" {
		t.Fatalf("with no stored token: got %+v, want gh's active account alice", a)
	}
	if _, err := secrets.Store(CopilotTokenEnv, "stored-tok", false); err != nil {
		t.Fatal(err)
	}
	if v, src, ok := secrets.Get(CopilotTokenEnv); !ok || src != "file" || v != "stored-tok" {
		t.Fatalf("secrets.Get(%s) = (%q, %q, %v), want (stored-tok, file, true)", CopilotTokenEnv, v, src, ok)
	}
	a, ok = copilotAuthSource()
	if !ok || a.Source != SrcStoredToken || a.token != "stored-tok" {
		t.Fatalf("with a stored token: got %+v, want Source=%s token=stored-tok", a, SrcStoredToken)
	}
}

// A pinned gh user (COPILOT_GITHUB_USER) still outranks a stored token.
func TestCopilotAuthSourcePinOutranksStoredToken(t *testing.T) {
	copilotAuthEnv(t)
	if _, err := secrets.Store(CopilotTokenEnv, "stored-tok", false); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinGitHubUser: "alice"}); err != nil {
		t.Fatal(err)
	}
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
	sub, err := reg.Get("subscription")
	if err != nil {
		t.Fatal(err)
	}
	sel := providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
	target, note, err := ResolveTarget(reg, "gemini", sel, false)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider == nil || target.Provider.Name != "gemini" || !strings.Contains(note, "Google login") {
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
func writeGatewayOverlay(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "env.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"providers":{"gateway":{"kind":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "env.d", "providers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
