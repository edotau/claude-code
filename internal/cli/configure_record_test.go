package cli

import (
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
)

// agents record refuses a bad key, agent or profile, writes nothing on --dry-run, and writes the record otherwise.
func TestAgentsRecord(t *testing.T) {
	hosttest.PinScratch(t)
	for _, args := range [][]string{
		{"codex", "bogus=1"}, {"nope", "model=x"}, {"codex", "fallback=nosuch"}, {"codex"},
	} {
		if code := cmdAgentsRecord(args); code == 0 {
			t.Errorf("record %v exited 0, want a refusal", args)
		}
	}
	if code := cmdAgentsRecord([]string{"codex", "model=gpt-x", "--dry-run"}); code != 0 || vault.GetAgentRecord("codex").Model != "" {
		t.Fatalf("dry-run = exit %d, model %q, want 0 and nothing written", code, vault.GetAgentRecord("codex").Model)
	}
	if code := cmdAgentsRecord([]string{"codex", "model=gpt-x"}); code != 0 || vault.GetAgentRecord("codex").Model != "gpt-x" {
		t.Errorf("record = exit %d, model %q, want 0 and gpt-x", code, vault.GetAgentRecord("codex").Model)
	}
}
