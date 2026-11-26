package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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
		{[]string{"--resume", id, "--profile", "gw", "--model", "claude-opus-4-6"}, "gw"},
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
	record, err := readSessionRecord(flagArg(fenced.Argv, "--session-id"))
	if err != nil || record.ov.Fence == nil || !record.ov.Fence.Allows("ollama") {
		t.Fatalf("recorded fence = %+v, %v", record.ov.Fence, err)
	}
	for _, tc := range []struct{ name, id, dispatch, now, want string }{
		{"fenced leaf keeps its fence", flagArg(fenced.Argv, "--session-id"), "", stray, record.fenceDoc},
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

// A resume retains the resolved model after the profile's alias changes.
func TestResumeKeepsDefaultAlias(t *testing.T) {
	hermetic(t, twoProfiles)
	_, fresh, err := planLaunch("/opt/claude", []string{"--profile", "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	id := flagArg(fresh.Argv, "--session-id")
	if rec, err := readSessionRecord(id); err != nil || rec.ov.Model != "qwen3-coder" {
		t.Fatalf("recorded facts = %+v (err %v), want model qwen3-coder", rec.ov, err)
	}
	if got := fresh.Set["ANTHROPIC_MODEL"]; got != "qwen3-coder" {
		t.Fatalf("fresh ANTHROPIC_MODEL = %q", got)
	}
	vaulttest.SetRecord(t, "claude", "aliases.opus", "qwen3-next") // the profile's opus changes after launch
	_, resumed, err := planLaunch("/opt/claude", []string{"--resume", id})
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.Set["ANTHROPIC_MODEL"]; got != "qwen3-coder" {
		t.Errorf("resume ANTHROPIC_MODEL = %q, want qwen3-coder", got)
	}
	if got := resumed.Set["CLAUDE_PROFILE"]; got != "ollama" {
		t.Errorf("resume profile = %q", got)
	}
}

// A fresh launch saves its launch overlay (provider, profile, model); resume derives from it after vault switch.
func TestSessionOverlayResume(t *testing.T) {
	hermetic(t, twoProfiles)
	t.Setenv("TEST_GW_KEY", "secret-value")
	_, fresh, err := planLaunch("/opt/claude", []string{"--overlay", `{"provider":"openai","profile":"ollama","model":"sonnet"}`})
	if err != nil {
		t.Fatal(err)
	}
	id := flagArg(fresh.Argv, "--session-id")
	rec, err := readSessionRecord(id)
	ov := rec.ov
	if err != nil || ov.Provider != "openai" || ov.Profile != "ollama" || ov.Model != "qwen3-coder" {
		t.Fatalf("saved launch overlay = %+v (err %v)", ov, err)
	}
	vaulttest.SetRecord(t, "claude", "profile", "gw")
	t.Setenv("ANTHROPIC_MODEL", "should-not-win")
	_, resumed, err := planLaunch("/opt/claude", []string{"-r", id})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Set["CLAUDE_PROFILE"] != "ollama" || resumed.Set["ANTHROPIC_MODEL"] != "qwen3-coder" {
		t.Fatalf("resumed set = %v, want ollama/qwen3-coder", resumed.Set)
	}
}

func TestResumeRecordIntegrity(t *testing.T) {
	hermetic(t, twoProfiles)
	if err := os.MkdirAll(filepath.Dir(factsPath(unknownID)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, data string
		wantError  bool
	}{
		{"legacy", `{"profile":"ollama","model":"qwen3-coder","fence":""}`, false},
		{"invalid json", `{`, true},
		{"wrong provider", `{"provider":"anthropic","profile":"ollama"}`, true},
		{"partial route", `{"profile":"ollama","endpoint":"proxy"}`, true},
		{"invalid endpoint", `{"provider":"openai","profile":"ollama","endpoint":"http://example.com"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(factsPath(unknownID), []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			var opts agent.LaunchOptions
			_, _, _, err := prepareSession(&opts, []string{"-r", unknownID})
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError && (opts.Route.Provider != "openai" || opts.Provider != "ollama") {
				t.Fatalf("legacy resume = %+v", opts)
			}
		})
	}
}

func TestResumeRestoresRouteAndFence(t *testing.T) {
	hermetic(t, twoProfiles)
	limit := 3
	ov := agentconfig.Overlay{Provider: "openai", Profile: "ollama", Model: "qwen3-coder", Endpoint: "http://127.0.0.1:1/v1",
		MaxDepth: &limit, Fence: &route.Fence{Profiles: []string{"ollama"}}}
	if err := writeLaunchOverlay(unknownID, ov); err != nil {
		t.Fatal(err)
	}
	var opts agent.LaunchOptions
	_, _, pin, err := prepareSession(&opts, []string{"-r", unknownID})
	if err != nil || pin == nil || opts.Route.Endpoint != ov.Endpoint {
		t.Fatalf("resume = %+v, pin = %+v, err = %v", opts, pin, err)
	}
	if !strings.Contains(pin.fenceDoc, `"maxDepth":3`) || !strings.Contains(pin.fenceDoc, `"fence":`) {
		t.Fatalf("resumed fence document = %s", pin.fenceDoc)
	}
}

func TestResumeOverrideKeepsFence(t *testing.T) {
	hermetic(t, twoProfiles)
	ov := agentconfig.Overlay{Provider: "openai", Profile: "ollama", Model: "qwen3-coder",
		Fence: &route.Fence{Profiles: []string{"ollama"}}}
	if err := writeLaunchOverlay(unknownID, ov); err != nil {
		t.Fatal(err)
	}
	model := agent.LaunchOptions{Model: "qwen3-next", ModelFlag: true}
	_, _, pin, err := prepareSession(&model, []string{"-r", unknownID})
	if err != nil || pin == nil || model.Model != "qwen3-next" || !strings.Contains(pin.fenceDoc, `"fence"`) {
		t.Fatalf("model override = %+v, pin = %+v, err = %v", model, pin, err)
	}
	profile := agent.LaunchOptions{Provider: "gw", ProfileFlag: true}
	if _, _, _, err := prepareSession(&profile, []string{"-r", unknownID}); err == nil {
		t.Fatal("profile override escaped the recorded fence")
	}
}

func TestSessionRecordsTypedOverlayCaps(t *testing.T) {
	hermetic(t, twoProfiles)
	_, plan, err := planLaunch("/opt/claude", []string{"--overlay", `{"provider":"openai","profile":"ollama","maxDepth":2}`})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := readSessionRecord(flagArg(plan.Argv, "--session-id"))
	if err != nil || rec.ov.MaxDepth == nil || *rec.ov.MaxDepth != 2 {
		t.Fatalf("saved overlay = %+v, err %v", rec.ov, err)
	}
}

func TestSessionRecordWriteError(t *testing.T) {
	hermetic(t, twoProfiles)
	dir := filepath.Dir(factsPath(unknownID))
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planLaunch("/opt/claude", []string{"--profile", "ollama"}); err == nil {
		t.Fatal("launch succeeded after the session record write failed")
	}
}
