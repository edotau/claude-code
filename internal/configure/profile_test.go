package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude and codex rows ApplyProfile and AgentPins need
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given codex already on qa and a stale last-used entry for both agents, ApplyProfile moves only claude, keeps the
// model pin, and forgets last-used for all targeted agents.
func TestApplyProfileChangesOnlyDifferingAgents(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"oauth"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}}}}}`)
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "qa"}); err != nil {
		t.Fatal(err)
	}
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Profile: "dev", Model: "pinned"}); err != nil {
		t.Fatal(err)
	}
	providers.RecordLastProfile("claude", "stale-claude")
	providers.RecordLastProfile("codex", "stale-codex")
	changed, err := ApplyProfile([]string{"claude", "codex"}, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"claude"}) {
		t.Errorf("changed = %v, want [claude]", changed)
	}
	recs := vault.AgentRecords(agentpkg.RosterNames())
	if recs["claude"].Profile != "qa" || recs["claude"].Model != "pinned" || recs["codex"].Profile != "qa" {
		t.Errorf("records = %+v", recs)
	}
	raw, _ := os.ReadFile(filepath.Join(paths.StateDir(), "last-profile.json"))
	if bytes.Contains(raw, []byte("stale-claude")) || bytes.Contains(raw, []byte("stale-codex")) {
		t.Errorf("last-used = %s", raw)
	}
}
