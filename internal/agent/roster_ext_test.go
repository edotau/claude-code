package agent_test

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
)

func TestRoster(t *testing.T) {
	if got := agent.Names(); !slices.Equal(got, []string{"codex", "gemini", "opencode", "copilot"}) {
		t.Errorf("external roster = %v, want [codex gemini opencode copilot]", got)
	}
}

// TestLookupKnownVendors: every external vendor is a registered, external schema with an engine of its own;
// resume, catalog and read-permission facts are pinned by TestRosterFacts.
func TestLookupKnownVendors(t *testing.T) {
	var engines []string
	for _, e := range agent.Engines() {
		engines = append(engines, e.Name())
	}
	for _, name := range []string{"codex", "gemini", "copilot", "opencode"} {
		s, ok := agent.LookupSchema(name)
		if !ok {
			t.Errorf("LookupSchema(%q) not found", name)
			continue
		}
		if s.Name != name || !s.External() {
			t.Errorf("LookupSchema(%q) = %q, external %v", name, s.Name, s.External())
		}
		if !slices.Contains(engines, name) {
			t.Errorf("schema %q has no engine", name)
		}
	}
}
