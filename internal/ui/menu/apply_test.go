package menu

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/configure"

	"github.com/edotau/claude-code/internal/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

// Given codex already on qa and a stale last-used entry for both agents, ApplyProfile moves only claude, keeps the
// model pin, and forgets last-used for all targeted agents.
func TestApplyProfileChangesOnlyDifferingAgents(t *testing.T) {
	d := dbxEnv(t)
	t.Chdir(t.TempDir())
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "qa"}); err != nil {
		t.Fatal(err)
	}
	if err := vault.SetAgentRecord("claude", vault.AgentRecord{Profile: "dev", Model: "pinned"}); err != nil {
		t.Fatal(err)
	}
	agentpkg.RecordLastProfile("claude", "stale-claude", nil)
	agentpkg.RecordLastProfile("codex", "stale-codex", nil)
	_ = d
	changed, err := configure.ApplyProfile([]string{"claude", "codex"}, "qa")
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
