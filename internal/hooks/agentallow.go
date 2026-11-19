package hooks

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// agentBashAllow allows an agent's blocked or asked Bash call with a note; only the AI-footer check still stops it.
func agentBashAllow(in *core.Input, r core.Result) core.Result {
	if in == nil || r.Decision == core.Allow || !strings.EqualFold(in.CanonicalTool(), "Bash") {
		return r
	}
	if in.AgentID == "" && os.Getenv(agent.DispatchPermissionsEnv) == "" {
		return r
	}
	if strings.Contains(r.Reason, "AI-attribution footer") {
		return r
	}
	r.Context = core.JoinContext(r.Context, "allowed for an agent (owner order); was: "+strings.TrimSpace(r.Reason))
	r.Decision, r.Reason, r.Verdict = core.Allow, "", core.VerdictPassed
	return r
}
