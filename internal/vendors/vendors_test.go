package vendors

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

const secret = "sk-SECRET-123"

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
			wantArgv: []string{"-m", "gemini-2.5-pro", "-p", "hi"},
			wantEnv:  map[string]string{"GEMINI_API_KEY": secret, "GEMINI_MODEL": "gemini-2.5-pro", "GOOGLE_GEMINI_BASE_URL": ""}},
		{vendor: "gemini", provider: "openai", model: "gpt-5", wantErr: "no gemini route"},
		{vendor: "opencode", provider: "openai", model: "gpt-5", h: &Headless{Prompt: "hi", SessionID: "ses", Effort: "max", Tools: true},
			wantArgv: []string{"run", "-m", "harness-openai/gpt-5", "--session", "ses", "--variant", "max", "hi"},
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

func TestOpenCodeConfigShape(t *testing.T) {
	tg := target(t, "openrouter", "anthropic/claude-opus-5")
	env, argv, err := ConfigureHeadless(context.Background(), "opencode", tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model      string                    `json:"model"`
		Permission map[string]string         `json:"permission"`
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
	if cfg.Permission["bash"] != "deny" || cfg.Model != "harness-openrouter/anthropic/claude-opus-5" {
		t.Errorf("tools-off permission/model wrong: %+v", cfg)
	}
	if strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], secret) || strings.Contains(strings.Join(argv, " "), secret) {
		t.Error("secret inlined into config or argv")
	}
}
