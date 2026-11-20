package hooks

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/permission"
)

// agentBashAllow allows an agent's blocked or asked call with a note; Floor blocks stay.
func agentBashAllow(in *core.Input, r core.Result) core.Result {
	if in == nil || r.Decision == core.Allow || !agentAllowTool(in.CanonicalTool()) {
		return r
	}
	if in.AgentID == "" && os.Getenv(agent.DispatchPermissionsEnv) == "" {
		return r
	}
	if r.Floor {
		return r
	}
	r.Context = core.JoinContext(r.Context, "allowed for an agent (owner order): "+allowedRule(r))
	r.Decision, r.Reason, r.Verdict = core.Allow, "", core.VerdictPassed
	return r
}

// agentAllowTool is Bash, or an edit tool when the dispatch runs under --permissions allow.
func agentAllowTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash":
		return true
	case "write", "edit", "multiedit":
		var p permission.Permissions
		return json.Unmarshal([]byte(os.Getenv(agent.DispatchPermissionsEnv)), &p) == nil && p.Mode == permission.PermAllow
	}
	return false
}

// allowedRule names the rule that was overridden, else the reason's first 60 runes.
func allowedRule(r core.Result) string {
	if r.Rule != "" {
		return r.Rule
	}
	s := []rune(strings.TrimSpace(r.Reason))
	if len(s) > 60 {
		s = s[:60]
	}
	return string(s)
}
