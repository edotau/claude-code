package agentconfig

import (
	"strings"
	"testing"
)

// TestShellSummaryNamesEveryCommand: the generated prose names every command the copilot shell policy admits.
func TestShellSummaryNamesEveryCommand(t *testing.T) {
	p := ShellPolicy()
	if len(p.Commands) == 0 || len(p.HookExtras) == 0 || len(p.CLIDeny) == 0 {
		t.Fatalf("embedded shell policy failed to parse or is empty: %+v", p)
	}
	for _, rs := range []ShellRules{p.Commands, p.HookExtras} {
		summary := rs.Summary()
		for _, r := range rs {
			for _, c := range r.Commands {
				if !strings.Contains(summary, c) {
					t.Errorf("summary %q does not name %q", summary, c)
				}
			}
		}
	}
}
