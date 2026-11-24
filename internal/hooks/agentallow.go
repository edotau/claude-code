package hooks

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/wthook"
)

// placementRun and claimCheckRun keep the leaf lift: a leaf's Bash block or ask from the module is allowed with a note.
func placementRun(in *core.Input) core.Result  { return leafBashAllow(docs.Placement, in) }
func claimCheckRun(in *core.Input) core.Result { return leafBashAllow(wthook.ClaimCheck, in) }

// leafBashAllow is run(in), except a leaf's Bash block or ask (not a Floor block) becomes allow plus a note.
func leafBashAllow(run core.Runner, in *core.Input) core.Result {
	r := run(in)
	if r.Decision == core.Allow || r.Floor || !strings.EqualFold(in.CanonicalTool(), "bash") {
		return r
	}
	if in.AgentID == "" && os.Getenv(agent.DispatchPermissionsEnv) == "" {
		return r
	}
	note := strings.TrimSpace(r.Reason)
	if r.Rule != "" && note != "" {
		note = r.Rule + "\n" + note
	} else {
		note = r.Rule + note
	}
	r.Context = core.JoinContext(r.Context, "allowed for an agent (owner order): "+note)
	r.Decision, r.Reason, r.Verdict = core.Allow, "", core.VerdictPassed
	return r
}
