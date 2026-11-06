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
	if got := agent.Names(); !slices.Equal(got, []string{"codex", "copilot", "gemini", "opencode"}) {
		t.Errorf("external roster = %v, want [codex copilot gemini opencode]", got)
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

// TestRosterPromptTransport: every uncapped row names its transport (the zero value is the capped argv),
// and gemini's verified ceiling sits below its stdin transport.
func TestRosterPromptTransport(t *testing.T) {
	want := map[string]agent.PromptTransport{"claude": agent.PromptStdin, "codex": agent.PromptStdin,
		"copilot": agent.PromptMessage, "opencode": agent.PromptArgv, "gemini": agent.PromptStdin}
	for name, via := range want {
		s, ok := agent.LookupSchema(name)
		if !ok || s.CLI.PromptVia != via {
			t.Errorf("%s PromptVia = %v (found %v), want %v", name, s.CLI.PromptVia, ok, via)
		}
	}
	if agent.PromptArgv.MaxBytes() != agent.MaxPromptBytes || agent.PromptStdin.MaxBytes() != 0 {
		t.Errorf("MaxBytes: argv %d stdin %d", agent.PromptArgv.MaxBytes(), agent.PromptStdin.MaxBytes())
	}
	if g, _ := agent.LookupSchema("gemini"); g.CLI.PromptMaxBytes != 100*1024 {
		t.Errorf("gemini PromptMaxBytes = %d, want %d", g.CLI.PromptMaxBytes, 100*1024)
	}
}
