package claude

import (
	"testing"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const unknownID = "00000000-0000-4000-8000-000000000000"

const twoProfiles = `{"fallback": [], "providers": {"anthropic": {"gw": {"protocol": "anthropic", "base_url": "https://gw.example/anthropic",
	"auth": {"type":"key", "env": "TEST_GW_KEY"}, "models": {"opus": "claude-opus-4-6"}}},
	"openai": {"ollama": {"protocol": "openai", "base_url": "http://127.0.0.1:1/v1", "auth": {"type": "none"},
	"models": {"opus": "qwen3-coder", "sonnet": "qwen3-coder", "haiku": "qwen3-coder"}}}}}`

// A session launched on ollama resumes on ollama after `switch gw`; a typed --profile on the resume line wins.
func TestResumeKeepsLaunchProfile(t *testing.T) {
	hermetic(t, twoProfiles)
	t.Setenv("TEST_GW_KEY", "secret-value")
	_, fresh, err := planLaunch("/opt/claude", []string{"--profile", "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	id := flagArg(fresh.Argv, "--session-id")
	if id == "" {
		t.Fatalf("fresh launch argv = %v, want --session-id <uuid>", fresh.Argv)
	}
	vaulttest.SetRecord(t, "claude", "profile", "gw") // switch gw, in another terminal

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--resume", id}, "ollama"},
		{[]string{"--resume", id, "--profile", "gw"}, "gw"},
		{[]string{"--resume", unknownID, "-r" + id}, "ollama"}, // the last selector is the one claude opens
		{[]string{"--resume", unknownID}, "gw"},                // an id with no record: today's stored profile
	} {
		_, plan, err := planLaunch("/opt/claude", tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.Set["CLAUDE_PROFILE"]; got != tc.want {
			t.Errorf("resume %v: CLAUDE_PROFILE = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A resume ignores the opening terminal's endpoint and overlay; a recorded fence returns, a live dispatch's stays.
func TestResumeIgnoresStrayRouteEnv(t *testing.T) {
	hermetic(t, twoProfiles)
	const fence, stray = `{"provider":"openai","profile":"ollama"}`, `{"provider":"anthropic","profile":"gw"}`
	t.Setenv(route.OverlayVar, fence) // the fenced leaf's own launch
	_, fenced, err := planLaunch("/opt/claude", []string{"--profile", "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(route.OverlayVar, "")
	_, open, err := planLaunch("/opt/claude", []string{"--profile", "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(proc.EnvAnthropicBaseURL, "http://stray.example")
	for _, tc := range []struct{ name, id, dispatch, now, want string }{
		{"fenced leaf keeps its fence", flagArg(fenced.Argv, "--session-id"), "", stray, fence},
		{"unfenced session drops a stray overlay", flagArg(open.Argv, "--session-id"), "", stray, ""},
		{"inside a dispatch the parent's overlay stays", flagArg(open.Argv, "--session-id"), "d1", stray, stray},
	} {
		t.Setenv(route.OverlayVar, tc.now)
		t.Setenv(proc.DispatchIDEnv, tc.dispatch)
		_, plan, err := planLaunch("/opt/claude", []string{"--resume", tc.id})
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if got := plan.Values[route.OverlayVar]; got != tc.want {
			t.Errorf("%s: overlay = %q, want %q", tc.name, got, tc.want)
		}
		if plan.Values[proc.EnvAnthropicBaseURL] != "" || plan.Set[proc.EnvAnthropicBaseURL] == "http://stray.example" {
			t.Errorf("%s: the terminal's ANTHROPIC_BASE_URL reached the resume", tc.name)
		}
	}
}

// A launch without --model records the default alias; a resume replays it after the profile's default id changes.
func TestResumeKeepsDefaultAlias(t *testing.T) {
	hermetic(t, twoProfiles)
	_, fresh, err := planLaunch("/opt/claude", []string{"--profile", "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	id := flagArg(fresh.Argv, "--session-id")
	if f, ok := readFacts(id); !ok || f.Model != "opus" {
		t.Fatalf("recorded facts = %+v (ok %v), want model opus", f, ok)
	}
	if got := fresh.Set["ANTHROPIC_MODEL"]; got != "qwen3-coder" {
		t.Fatalf("fresh ANTHROPIC_MODEL = %q", got)
	}
	vaulttest.SetRecord(t, "claude", "aliases.opus", "qwen3-next") // the profile's opus changes after launch
	_, resumed, err := planLaunch("/opt/claude", []string{"--resume", id})
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.Set["ANTHROPIC_MODEL"]; got != "qwen3-next" || resumed.Set["ANTHROPIC_DEFAULT_OPUS_MODEL"] != got {
		t.Errorf("resume ANTHROPIC_MODEL = %q, want the opus alias replayed on the same profile (qwen3-next)", got)
	}
	if got := resumed.Set["CLAUDE_PROFILE"]; got != "ollama" {
		t.Errorf("resume profile = %q", got)
	}
}
