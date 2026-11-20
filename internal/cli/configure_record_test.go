package cli

import (
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
)

// configure agents refuses a bad key, agent or profile, writes nothing on --dry-run, and writes the record otherwise.
func TestConfigureRecord(t *testing.T) {
	hosttest.PinScratch(t)
	for _, args := range [][]string{
		{"codex", "bogus=1"}, {"nope", "model=x"}, {"codex", "fallback=nosuch"},
		{"codex", "config_route=nosuch"}, {"gemini", "config_route=session"},
	} {
		if code := cmdConfigure(append([]string{"agents"}, args...)); code == 0 {
			t.Errorf("record %v exited 0, want a refusal", args)
		}
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=gpt-x", "--dry-run"}); code != 0 || vault.GetAgentRecord("codex").Model != "" {
		t.Fatalf("dry-run = exit %d, model %q, want 0 and nothing written", code, vault.GetAgentRecord("codex").Model)
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=gpt-x", "--check"}); code != 0 || vault.GetAgentRecord("codex").Model != "" {
		t.Fatalf("--check = exit %d, model %q, want 0 and nothing written", code, vault.GetAgentRecord("codex").Model)
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=gpt-x"}); code != 0 || vault.GetAgentRecord("codex").Model != "gpt-x" {
		t.Errorf("record = exit %d, model %q, want 0 and gpt-x", code, vault.GetAgentRecord("codex").Model)
	}
}

// config_route is record-only: a dry run writes nothing, a real run stores it in the codex record's extras.
func TestConfigureRecordRoute(t *testing.T) {
	hosttest.PinScratch(t)
	if code := cmdConfigure([]string{"agents", "codex", "config_route=session", "--dry-run"}); code != 0 ||
		vault.GetAgentRecord("codex").Extras["config_route"] != "" {
		t.Fatalf("config_route dry-run = exit %d, want 0 and nothing written", code)
	}
	if code := cmdConfigure([]string{"agents", "codex", "config_route=session"}); code != 0 ||
		vault.GetAgentRecord("codex").Extras["config_route"] != "session" {
		t.Errorf("config_route = exit %d, extras %v, want 0 and session", code, vault.GetAgentRecord("codex").Extras)
	}
}
