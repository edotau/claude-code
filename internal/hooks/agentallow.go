package hooks

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// agentBashAllow allows an agent's blocked or asked call with a note; only the AI-footer check still stops it.
func agentBashAllow(in *core.Input, r core.Result) core.Result {
	if in == nil || r.Decision == core.Allow || !agentAllowTool(in.CanonicalTool()) {
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

// agentAllowTool is Bash, or an edit tool when the dispatch runs under --permissions allow.
func agentAllowTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash":
		return true
	case "write", "edit", "multiedit":
		var p agent.Permissions
		return json.Unmarshal([]byte(os.Getenv(agent.DispatchPermissionsEnv)), &p) == nil && p.Mode == agent.PermAllow
	}
	return false
}
