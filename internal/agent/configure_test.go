package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
