package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

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
			env, argv, err := schemaFor(c.vendor).Configure(context.Background(), tg, "")
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

	// opencode's own GitHub Copilot login (ErrOwnLogin on the interactive dispatch bypasses Configure
	// entirely) still carries the spec through the exported LaunchSpec escape hatch.
	if env, _ := schemaFor("opencode").LaunchSpec(); !slices.Contains(env, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1") {
		t.Errorf("LaunchSpec(opencode) env = %q, want to contain OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1", env)
	}
}

// TestHeadlessDispatchesGetNoLaunchArgs: a tools-off headless dispatch must never pick up a vendor's allow-all /
// skip-permissions launch arg — that would silently re-enable tools the caller explicitly turned off.
func TestHeadlessDispatchesGetNoLaunchArgs(t *testing.T) {
	tg := target(t, "personal", "claude-opus-5.5")
	_, argv, err := schemaFor("copilot").configureHeadless(context.Background(), tg, Headless{Prompt: "hi", Tools: false})
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
	_, argv, err = schemaFor("gemini").configureHeadless(context.Background(), gtg, Headless{Prompt: "hi", Tools: false})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--dangerously-skip-permissions") {
		t.Errorf("gemini headless tools-off argv = %q, must not contain --dangerously-skip-permissions", argv)
	}
}
