package agent

import (
	"context"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a recorded workspace profile, a Claude launch resolves flag > last-used > pin > registry default.
func TestClaudeProfileResolutionLastUsed(t *testing.T) {
	hermetic(t, "")
	ws := t.TempDir()
	t.Chdir(ws)
	profile := func(o LaunchOptions) string {
		t.Helper()
		plan, err := Claude(context.Background(), o, nil)
		if err != nil {
			t.Fatal(err)
		}
		return plan.Set[providers.EnvSessionProfile]
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Fatalf("no record: got %q, want the registry default max", got)
	}
	RecordLastProfile("claude", "nvidia", nil)
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("last-used: got %q, want nvidia", got)
	}
	RecordLastProfile("claude", "anthropic", []string{"-p", "x"})
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("headless run recorded: got %q, want nvidia", got)
	}
	if got := profile(LaunchOptions{Provider: "anthropic"}); got != "anthropic" {
		t.Errorf("flag: got %q, want anthropic", got)
	}
	vaulttest.SetRecord(t, "claude", "profile", "max")
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("pin set: got %q, want nvidia (this workspace beats the pin)", got)
	}
	if got := providers.AgentRecord("claude").Profile; got != "max" {
		t.Errorf("recording must not touch the pin, got %q", got)
	}
	if err := ClearLastProfiles(); err != nil {
		t.Fatal(err)
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Errorf("after switch clears the cache: got %q, want the pin max", got)
	}
	vaulttest.SetRecord(t, "claude", "profile", "")
	if err := os.WriteFile(lastProfileFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Errorf("corrupt file: got %q, want the default", got)
	}
}

// Given a claude entry, an agent CLI launch ignores it; its own entry beats the agent pin, and the pin setter clears only it.
func TestAgentLastProfileIsPerAgent(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "nvidia")
	vaulttest.SetRecord(t, "copilot", "profile", "nvidia")
	resolved := func(name string) string { return launchProfile(name, LaunchOptions{}).Provider }
	RecordLastProfile("claude", "anthropic", nil)
	RecordLastProfile("opencode", "openai", nil)
	if got := resolved("opencode"); got != "openai" {
		t.Errorf("opencode entry: got %q, want openai (beats the nvidia pin)", got)
	}
	if got := resolved("copilot"); got != "nvidia" {
		t.Errorf("copilot read another agent's entry: got %q, want its nvidia pin", got)
	}
	if got := launchProfile("opencode", LaunchOptions{Provider: "nvidia"}).Provider; got != "nvidia" {
		t.Errorf("flag: got %q, want nvidia", got)
	}
	if err := ClearLastProfilesFor("opencode"); err != nil {
		t.Fatal(err)
	}
	if got := resolved("opencode"); got != "nvidia" {
		t.Errorf("after the opencode pin setter: got %q, want the nvidia pin", got)
	}
	if got := readLastProfiles().Workspaces[workspaceKey()]["claude"].Profile; got != "anthropic" {
		t.Errorf("claude entry lost by the opencode setter: %q", got)
	}
}

// Given a v1 file (one entry per workspace), decoding yields that entry under "claude".
func TestLastProfileMigratesV1ToClaude(t *testing.T) {
	raw := []byte(`{"schema":1,"workspaces":{"/w/a":{"profile":"prod","updated":"2026-01-02T03:04:05Z"}}}`)
	var st lastProfiles
	if _, err := schemas.Decode(lastProfileFormat, raw, &st); err != nil {
		t.Fatal(err)
	}
	if got := st.Workspaces["/w/a"]["claude"].Profile; got != "prod" || len(st.Workspaces["/w/a"]) != 1 {
		t.Errorf("v1 entry: got %+v, want claude=prod only", st.Workspaces)
	}
}

// Given codex records, its launch reuses the repo's last profile (own-login chatgpt included) and an off codex stays off.
func TestCodexReusesLastProfileButOffStaysOff(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	if err := SetAgentRecord("codex", AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	RecordLastProfile("codex", "chatgpt", nil)
	if got := launchProfile("codex", LaunchOptions{}).Provider; got != "chatgpt" {
		t.Errorf("on codex: got %q, want the repo's last-used chatgpt over its record", got)
	}
	if err := SetAgentRecord("codex", AgentRecord{}); err != nil {
		t.Fatal(err)
	}
	if got := launchProfile("codex", LaunchOptions{}).Provider; got != "" {
		t.Errorf("off codex: got %q, the cache must never turn it on", got)
	}
}
