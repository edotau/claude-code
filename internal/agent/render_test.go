package agent

import (
	"slices"
	"testing"
)

// Ties DefaultAgents to the render order, which shows in generate's output.
func TestDefaultAgentsOrder(t *testing.T) {
	if want := []string{"codex", "opencode", "gemini", "copilot"}; !slices.Equal(DefaultAgents, want) {
		t.Errorf("DefaultAgents = %v, want %v", DefaultAgents, want)
	}
}
