package vendors

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

const secret = "sk-SECRET-123"

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"} {
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
			wantArgv: []string{"exec", `model_reasoning_effort="high"`, "--skip-git-repo-check", "--sandbox", "read-only", "resume", "s1", "--", "-hi"}},
		{vendor: "codex", provider: "openrouter", model: "x", wantErr: "no responses route"},
		{vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro", h: &Headless{Prompt: "hi"},
			wantArgv: []string{"--skip-trust", "-m", "gemini-2.5-pro", "--output-format", "stream-json", "--approval-mode", "plan", "--prompt=hi"},
			wantEnv: map[string]string{"GEMINI_API_KEY": secret, "GEMINI_MODEL": "gemini-2.5-pro", "GOOGLE_GEMINI_BASE_URL": "",
				"GEMINI_API_KEY_AUTH_MECHANISM": "x-goog-api-key", "GOOGLE_GENAI_USE_GCA": "", "GEMINI_CLI_TRUST_WORKSPACE": ""}},
		{vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro",
			wantEnv: map[string]string{"GEMINI_CLI_TRUST_WORKSPACE": "true"}},
		{vendor: "gemini", provider: "gemini", model: "gemini-2.5-pro", h: &Headless{Prompt: "-hi", Tools: true, SessionID: "u1"},
			wantArgv: []string{"--approval-mode", "yolo", "--resume", "u1", "--prompt=-hi"}},
		{vendor: "gemini", provider: "openai", model: "gpt-5", wantErr: "no gemini route"},
		{vendor: "opencode", provider: "openai", model: "gpt-5", h: &Headless{Prompt: "hi", SessionID: "ses", Effort: "max", Tools: true},
			wantArgv: []string{"run", "-m", "harness-openai/gpt-5", "--session", "ses", "--variant", "max", "hi"},
			wantEnv:  map[string]string{OpenCodeKeyEnv: secret}},
		{vendor: "opencode", provider: "gemini", model: "gemini-3.6-flash", h: &Headless{Prompt: "hi"},
			wantArgv: []string{"run", "--format", "json", "-m", "harness-gemini/gemini-3.6-flash", "hi"},
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
	want := []string{"exec", "-c", `model_provider="openai"`, "-c", `model="gpt-5.5"`, "--skip-git-repo-check", "--sandbox", "read-only", "hi"}
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

func TestOpenCodeConfigShape(t *testing.T) {
	tg := target(t, "openrouter", "anthropic/claude-opus-5")
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
	prov := cfg.Provider["harness-openrouter"]
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/anthropic" || opts["baseURL"] != "https://openrouter.ai/api/v1" {
		t.Errorf("provider block = %v", prov)
	}
	if opts["apiKey"] != "{env:"+OpenCodeKeyEnv+"}" || opts["headers"].(map[string]any)["Authorization"] != "Bearer {env:"+OpenCodeKeyEnv+"}" {
		t.Errorf("auth must reference the env var, got %v", opts)
	}
	if cfg.Permission != "deny" || cfg.Model != "harness-openrouter/anthropic/claude-opus-5" {
		t.Errorf("tools-off permission/model wrong: %+v", cfg)
	}
	if strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], secret) || strings.Contains(strings.Join(argv, " "), secret) {
		t.Error("secret inlined into config or argv")
	}
}

func TestOpenCodeGeminiNative(t *testing.T) {
	tg := target(t, "gemini", "gemini-3.6-flash")
	env, _, err := Configure(context.Background(), "opencode", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Provider map[string]map[string]any `json:"provider"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	prov := cfg.Provider["harness-gemini"]
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/google" || opts["baseURL"] != "https://generativelanguage.googleapis.com/v1beta" {
		t.Errorf("gemini route must use the native SDK, got %v", prov)
	}
	if _, ok := opts["headers"]; ok {
		t.Errorf("Google's own host takes x-goog-api-key, not a bearer header: %v", opts["headers"])
	}
}

func TestGeminiGateway(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Kind: providers.DialectGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthBearer, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
	env, _, err := Configure(context.Background(), "gemini", providers.Target{Provider: p, Model: "m"}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := envMap(env)
	want := map[string]string{"GOOGLE_GEMINI_BASE_URL": "https://gw.example/gemini", "GEMINI_API_KEY_AUTH_MECHANISM": "bearer",
		"GEMINI_CLI_CUSTOM_HEADERS": "X-Team:a", "GEMINI_API_KEY": secret}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestGeminiAuthConflict(t *testing.T) {
	tg := target(t, "gemini", "gemini-2.5-pro")
	for body, wantErr := range map[string]bool{
		`{"security":{"auth":{"selectedType":"oauth-personal"}}}`: true,
		`{"selectedAuthType":"vertex-ai"}`:                        true,
		`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`: false,
		`{"theme":"Default"}`:                                     false,
	} {
		home := t.TempDir()
		t.Setenv("GEMINI_CLI_HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".gemini", "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := ConfigureHeadless(context.Background(), "gemini", tg, Headless{Prompt: "hi"})
		if (err != nil) != wantErr || (wantErr && !strings.Contains(err.Error(), "overrides the provider key")) {
			t.Errorf("%s: err = %v, wantErr %v", body, err, wantErr)
		}
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

func TestOpenCodeInteractiveImports(t *testing.T) {
	tg := target(t, "anthropic", "claude-sonnet-5")

	proj := t.TempDir()
	t.Chdir(proj)
	cwd, err := os.Getwd() // t.TempDir() may be under a /var -> /private/var symlink on macOS
	if err != nil {
		t.Fatal(err)
	}

	global := map[string]any{"permissions": map[string]any{
		"deny": []string{"Bash(rm -rf:*)"}, "ask": []string{"Bash(git push:*)"},
	}}
	writeJSON(t, filepath.Join(paths.ConfigDir(), "settings.json"), global)
	writeJSON(t, filepath.Join(paths.ConfigDir(), ".claude.json"), map[string]any{
		"mcpServers": map[string]any{
			"shared": map[string]any{"command": "global"},
			"g":      map[string]any{"url": "https://g"},
		},
		"projects": map[string]any{
			cwd: map[string]any{"enabledMcpjsonServers": []string{"shared"}},
		},
	})

	writeJSON(t, filepath.Join(proj, ".claude", "settings.json"), map[string]any{"permissions": map[string]any{
		"allow": []string{"Bash(make:*)", "Edit(*)"},
	}})
	writeJSON(t, filepath.Join(proj, ".claude", "settings.local.json"), map[string]any{"permissions": map[string]any{
		"ask": []string{"Bash(make:*)"},
	}})
	writeJSON(t, filepath.Join(proj, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"shared":     map[string]any{"command": "project"},
		"unapproved": map[string]any{"command": "nope"},
	}})

	env, _, err := Configure(context.Background(), "opencode", tg, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permission struct {
			Bash map[string]string `json:"bash"`
			Edit string            `json:"edit"`
		} `json:"permission"`
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	wantBash := map[string]string{"rm -rf *": "deny", "git push *": "ask", "make *": "ask"}
	for k, v := range wantBash {
		if cfg.Permission.Bash[k] != v {
			t.Errorf("permission.bash[%q] = %q, want %q", k, cfg.Permission.Bash[k], v)
		}
	}
	if cfg.Permission.Edit != "allow" {
		t.Errorf("permission.edit = %q, want allow", cfg.Permission.Edit)
	}
	if got := cfg.MCP["shared"].Command; len(got) != 1 || got[0] != "project" {
		t.Errorf("mcp.shared.command = %v, want [project] (project wins)", got)
	}
	if cfg.MCP["g"].Type != "remote" {
		t.Errorf("mcp.g.type = %q, want remote", cfg.MCP["g"].Type)
	}
	if _, ok := cfg.MCP["unapproved"]; ok {
		t.Errorf("mcp.unapproved must be absent (not in enabledMcpjsonServers): %v", cfg.MCP["unapproved"])
	}
	if got := envMap(env)["HARNESS_OPENCODE_MCP_SKIPPED"]; got != "unapproved" {
		t.Errorf("HARNESS_OPENCODE_MCP_SKIPPED = %q, want %q", got, "unapproved")
	}

	// A malformed .mcp.json must not fail the launch.
	if err := os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Configure(context.Background(), "opencode", tg, ""); err != nil {
		t.Fatalf("malformed .mcp.json must not fail the launch: %v", err)
	}

	env, _, err = ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var headlessCfg map[string]any
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &headlessCfg); err != nil {
		t.Fatal(err)
	}
	if headlessCfg["permission"] != "deny" {
		t.Errorf("headless permission = %v, want deny", headlessCfg["permission"])
	}
	if _, ok := headlessCfg["mcp"]; ok {
		t.Errorf("headless must carry no mcp key: %v", headlessCfg["mcp"])
	}

	env, _, err = ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	var toolsCfg map[string]any
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &toolsCfg); err != nil {
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
