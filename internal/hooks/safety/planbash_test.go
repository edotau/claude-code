package safety

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestPlanBashReadOnlyOnly(t *testing.T) {
	cases := []struct {
		agent, cmd string
		want       core.Decision
	}{
		{"Plan", "git log -1", core.Allow},
		{"Plan", "cd /tmp && git log -1", core.Allow},
		{"Plan", "rtk git log -1", core.Allow},
		{"Plan", "cd /tmp && rtk git log -1", core.Allow},
		{"Plan", "claude-code memory search plan", core.Allow},
		{"Plan", "touch /tmp/x", core.Block},
		{"Plan", "git log -1 > /tmp/x", core.Block},
		{"Plan", "cd /tmp && touch /tmp/x", core.Block},
		{"Plan", "cd /tmp > /tmp/x && git log -1", core.Block},
		{"Plan", "cd /tmp && git log -1 > /tmp/x", core.Block},
		{"Plan", "cd $(touch /tmp/x) && git log -1", core.Block},
		{"", "touch /tmp/x", core.Allow},
		{"claude", "touch /tmp/x", core.Allow},
	}
	for _, c := range cases {
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "agent_type": c.agent, "tool_input": map[string]string{"command": c.cmd}})
		if got, _, _ := planBashVerdict(core.ParseInput(strings.NewReader(string(b))), c.cmd); got != c.want {
			t.Errorf("verdict %s %q: %v, want %v", c.agent, c.cmd, got, c.want)
		}
		if got, msg := safetyScanDecision(t, strings.NewReader(string(b))); got != c.want {
			t.Errorf("%s %q: %v, want %v (%s)", c.agent, c.cmd, got, c.want, msg)
		}
	}
}
