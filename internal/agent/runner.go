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

func lastHopModel(res wire.Result) string {
	if n := len(res.Hops); n > 0 {
		return res.Hops[n-1].Model
	}
	return ""
}

// carriedFor: Pi ran asked's request, by the answering hop's typed RunFor, else by every hop having run as Pi.
func carriedFor(res wire.Result, asked string) bool {
	if n := len(res.Hops); n > 0 && slices.ContainsFunc(res.Hops, func(h wire.Hop) bool { return h.RunFor != "" }) {
		return res.Hops[n-1].RunFor == asked
	}
	return !slices.ContainsFunc(res.Hops, func(h wire.Hop) bool { return h.Agent != agentid.Pi })
}

// stampRunner sets the typed Runner, Asked and Type on res: Runner "pi" when Pi ran another agent's request.
func stampRunner(rec Record, res wire.Result) wire.Result {
	engine := engineOf(res)
	if engine == agentid.Pi && rec.Agent != agentid.Pi && carriedFor(res, rec.Agent) {
		res.Asked, res.Runner = rec.Agent, engine
	}
	codeMode := (rec.Request.Codemode == "on" || rec.Request.Codemode == "only") && engine == agentid.Pi
	model, kind := ranTarget(rec, res)
	res.Type = cmp.Or(string(models.AgentType(kind, model, codeMode)), res.Asked, res.Agent)
	if rec.Agent == agentid.Pi && engine == agentid.Pi && res.Type != agentid.Pi {
		res.Runner = engine // a pi ask outside code mode shows the model's type, carried by pi
	}
	return res
}

// ranTarget is the model that ran and its route's provider kind; "" for the auto model.
func ranTarget(rec Record, res wire.Result) (model, kind string) {
	model = cmp.Or(res.Model, lastHopModel(res), res.Invoked, rec.Request.Model)
	if model == AutoModel {
		model = ""
	}
	if len(res.Hops) == 0 {
		return model, rec.Route.Provider // no hop ran: the launch route is all there is
	}
	return model, ranRoute(res).Provider
}

// RowLabel is r's label on every row: its agent type, else the answering agent, with the runner after it.
func RowLabel(r Record) string {
	return wire.RunLabel(cmp.Or(r.Type, r.RanAs, r.Agent), RunnerOf(r))
}

// RunnerOf is the runner recorded for r's run; an older record names none and stays unclassified.
func RunnerOf(r Record) string { return r.Runner }
