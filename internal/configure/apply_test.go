package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

// Given codex already on qa and a stale last-used entry for both agents, ApplyProfile moves only claude, keeps the
// model pin, and forgets last-used for the changed agent alone.
func TestApplyProfileChangesOnlyDifferingAgents(t *testing.T) {
	d := dbxEnv(t)
	t.Chdir(t.TempDir())
	if err := agentpkg.SetAgentRecord("codex", agentpkg.AgentRecord{Profile: "qa"}); err != nil {
		t.Fatal(err)
	}
	if err := agentpkg.SetAgentRecord("claude", agentpkg.AgentRecord{Profile: "dev", Model: "pinned"}); err != nil {
		t.Fatal(err)
	}
	agentpkg.RecordLastProfile("claude", "stale-claude", nil)
	agentpkg.RecordLastProfile("codex", "stale-codex", nil)
	_ = d
	changed, err := ApplyProfile([]string{"claude", "codex"}, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"claude"}) {
		t.Errorf("changed = %v, want [claude]", changed)
	}
	recs := agentpkg.AgentRecords()
	if recs["claude"].Profile != "qa" || recs["claude"].Model != "pinned" || recs["codex"].Profile != "qa" {
		t.Errorf("records = %+v", recs)
	}
	raw, _ := os.ReadFile(filepath.Join(paths.StateDir(), "last-profile.json"))
	if bytes.Contains(raw, []byte("stale-claude")) || !bytes.Contains(raw, []byte("stale-codex")) {
		t.Errorf("last-used = %s", raw)
	}
}
