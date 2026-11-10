package agents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// A coworker leaf (pinned or bare with an Agent: line) is blocked with no ran-on tool result and released by one.
func TestRanOnGateCoworkerTypes(t *testing.T) {
	if _, blocked := leafStopBlock(leafTx(t, "coworker:copilot", "Objective: x\n", "ls output"), "a1"); !blocked {
		t.Error("coworker:copilot with no ran-on tool result must be blocked")
	}
	if _, blocked := leafStopBlock(leafTx(t, "coworker", "Objective: x\nAgent: opencode\n", "ls output"), "a1"); !blocked {
		t.Error("coworker with Agent: opencode and no ran-on tool result must be blocked")
	}
	in := leafTx(t, "coworker:copilot", "Objective: x\n", "ask: ran on copilot · acct · gpt-6.1-sol · fallback none")
	if r, blocked := leafStopBlock(in, "a1"); blocked || r.SystemMessage != "" {
		t.Errorf("coworker:copilot with a ran-on tool result: got %+v blocked=%v, want a silent release", r, blocked)
	}
}

// A coworker leaf's label is the routed model of the agent it runs: pinned type, Agent: line, or Profile:.
func TestCoworkerLabels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"dbx"}, true)
	stubLabelModels(t, "", "dbx-model")
	labelCell = func(agent, alias string) string { return agent + "-" + alias }
	for _, c := range []struct{ st, prompt, want string }{
		{"coworker:copilot", "fix x", "copilot-sonnet: task"},
		{"coworker:copilot", "Agent: codex\nfix x", "copilot-sonnet: task"},
		{"coworker:opencode", "fix x", "opencode-sonnet: task"},
		{"coworker", "fix x", "copilot-sonnet: task"},
		{"coworker", "Agent: opencode\nfix x", "opencode-sonnet: task"},
		{"coworker:databricks", "Agent: codex\nProfile: dbx\nfix x", "dbx-model: task"},
	} {
		payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":"task"}}`,
			c.st, c.prompt)
		r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
		if got, _ := r.UpdatedInput["description"].(string); got != c.want {
			t.Errorf("%s %q: label = %q, want %q", c.st, c.prompt, got, c.want)
		}
	}
}
