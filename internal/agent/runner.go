package agent

import (
	"cmp"
	"slices"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
)

// engineOf is the agent that ran res: its last hop's, else the result's own.
func engineOf(res wire.Result) string {
	if n := len(res.Hops); n > 0 && res.Hops[n-1].Agent != "" {
		return res.Hops[n-1].Agent
	}
	return res.Agent
}

// onlyPiHops: no hop ran as another agent, so Pi carried the request even across a workspace re-pick.
func onlyPiHops(res wire.Result) bool {
	return !slices.ContainsFunc(res.Hops, func(h wire.Hop) bool { return h.Agent != agentid.Pi })
}

// stampRunner sets the typed Runner, Asked and Type on res: Runner "pi" when Pi ran another agent's request.
func stampRunner(rec Record, res wire.Result) wire.Result {
	engine := engineOf(res)
	if engine == agentid.Pi && rec.Agent != agentid.Pi && onlyPiHops(res) {
		res.Asked, res.Runner = rec.Agent, engine
	}
	codeMode := (rec.Request.Codemode == "on" || rec.Request.Codemode == "only") && engine == agentid.Pi
	model := cmp.Or(res.Model, rec.Request.Model)
	if model == AutoModel {
		model = ""
	}
	kind := cmp.Or(ranRoute(res).Provider, rec.Route.Provider)
	res.Type = cmp.Or(string(models.AgentType(kind, model, codeMode)), res.Asked, res.Agent)
	return res
}

// RowLabel is r's label on every row: its agent type, else the asked agent, with the runner after it.
func RowLabel(r Record) string { return wire.RunLabel(cmp.Or(r.Type, r.Agent), RunnerOf(r)) }

// RunnerOf is the runner that carried r's run: the recorded one, else Pi read from the attempts of an older record.
func RunnerOf(r Record) string {
	if r.Runner != "" || r.Fallback != "" || len(r.Attempts) == 0 {
		return r.Runner
	}
	if last := r.Attempts[len(r.Attempts)-1].Agent; last == agentid.Pi && r.Agent != agentid.Pi {
		return last
	}
	return ""
}
