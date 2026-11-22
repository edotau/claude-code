package safety

import (
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/permission"
)

// planBashVerdict confines the Plan subagent's Bash to the read-only shell; a floor block for Plan only.
func planBashVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if in.Nested("agent_type") != "Plan" || permission.ReadOnlyShell(cmd) {
		return core.Allow, "", ""
	}
	return core.Block, core.SafetyMessage(in, core.Block, "the Plan agent runs read-only shell commands only", "", ""), "plan agent bash"
}
