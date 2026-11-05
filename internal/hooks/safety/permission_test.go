package safety

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// TestPermissionGate drives the gate with real hook JSON under each env shape the dispatch can hand a child.
func TestPermissionGate(t *testing.T) {
	const policy = `{"deny":[{"Tool":"Bash","Arg":"rm *"},{"Tool":"Edit","Arg":".env"}]}`
	cases := []struct {
		name, env, payload string
		block              bool
		reason             string
	}{
		{"unset env is a no-op", "", `{"tool_name":"Bash","tool_input":{"command":"rm x"}}`, false, ""},
		{"deny blocks rm", policy, `{"tool_name":"Bash","tool_input":{"command":"rm v1"}}`, true, "deny Bash(rm v1)"},
		{"deny blocks bash -c rm", policy, `{"tool_name":"Bash","tool_input":{"command":"bash -c \"rm v3\""}}`, true, `Bash(bash -c "rm v3")`},
		{"deny blocks a protected path in cwd", policy,
			`{"tool_name":"write","cwd":"/repo","tool_input":{"filePath":"/repo/.env","content":"k"}}`, true, "deny Edit(.env)"},
		{"allowed command passes", policy, `{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`, false, ""},
		{"malformed env blocks", `{deny`, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, true, "is unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(agent.DispatchPermissionsEnv, c.env)
			res := PermissionGate(core.ParseInput(strings.NewReader(c.payload)))
			if (res.Decision == core.Block) != c.block || !strings.Contains(res.Reason, c.reason) {
				t.Errorf("decision %v reason %q, want block=%v naming %q", res.Decision, res.Reason, c.block, c.reason)
			}
		})
	}
}
