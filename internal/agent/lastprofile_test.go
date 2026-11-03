package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a claude entry, an agent CLI launch ignores it; its own entry beats the agent pin, and the pin setter clears only it.
func TestAgentLastProfileIsPerAgent(t *testing.T) {
	hermetic(t, "")
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "opencode", "profile", "nvidia")
	vaulttest.SetRecord(t, "copilot", "profile", "nvidia")
	resolved := func(name string) string { return testLaunchProfile(t, name, LaunchOptions{}).Provider }
	RecordLastProfile("claude", "anthropic", nil)
	RecordLastProfile("opencode", "openai", nil)
	if got := resolved("opencode"); got != "openai" {
		t.Errorf("opencode entry: got %q, want openai (beats the nvidia pin)", got)
	}
	if got := resolved("copilot"); got != "nvidia" {
		t.Errorf("copilot read another agent's entry: got %q, want its nvidia pin", got)
	}
	if got := testLaunchProfile(t, "opencode", LaunchOptions{Provider: "nvidia"}).Provider; got != "nvidia" {
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
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	RecordLastProfile("codex", "chatgpt", nil)
	if got := testLaunchProfile(t, "codex", LaunchOptions{}).Provider; got != "chatgpt" {
		t.Errorf("on codex: got %q, want the repo's last-used chatgpt over its record", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{}); err != nil {
		t.Fatal(err)
	}
	if got := testLaunchProfile(t, "codex", LaunchOptions{}).Provider; got != "" {
		t.Errorf("off codex: got %q, the cache must never turn it on", got)
	}
}

// Given a gemini last-used gateway row over a different vault pin, a gemini launch resolves flag > last-used > pin.
func TestGeminiReusesLastProfile(t *testing.T) {
	hermetic(t, "")
	vaulttest.UseKeyring(t)
	row := func(n string) string {
		return `"` + n + `":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",` +
			`"auth":{"type":"key","command":"echo tok"},"models":{"opus":"databricks-claude-opus-5-5"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("gem-a")+`,`+row("gem-b")+`}}}`)
	t.Chdir(t.TempDir())
	vaulttest.SetRecord(t, "gemini", "profile", "gem-a")
	RecordLastProfile("gemini", "gem-b", nil)
	if got := testLaunchProfile(t, "gemini", LaunchOptions{}).Provider; got != "gem-b" {
		t.Errorf("gemini last-used: got %q, want gem-b over the gem-a pin", got)
	}
	if got := testLaunchProfile(t, "gemini", LaunchOptions{Provider: "gem-a"}).Provider; got != "gem-a" {
		t.Errorf("flag: got %q, want gem-a", got)
	}
}

func testLaunchProfile(t *testing.T, name string, o LaunchOptions) LaunchOptions {
	t.Helper()
	got, err := launchProfile(name, o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
