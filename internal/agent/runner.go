package agent

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/rtkwrap"
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
	if carriedByPi(rec, res, engine) {
		res.Asked, res.Runner = rec.Agent, engine
	}
	// the pi label is for a pi code-mode run only: a claude or opencode one keeps its own name
	codeMode := rtkwrap.CodemodeOn(rec.Request.Codemode) && agentid.Routing(engine).CodeMode.TypedByMode
	typedByMode := agentid.Routing(rec.Agent).CodeMode.TypedByMode
	if !typedByMode && !codeMode {
		res.Type = cmp.Or(res.Asked, res.Agent) // a typed agent is never relabelled by its model's family
		return res
	}
	model, kind := ranTarget(rec, res)
	res.Type = cmp.Or(string(models.AgentType(kind, model, codeMode)), res.Asked, res.Agent)
	if typedByMode && engine == rec.Agent && res.Type != rec.Agent {
		res.Runner = engine // a pi ask outside code mode shows the model's type, carried by pi
	}
	return res
}

// carriedByPi: Pi ran another agent's request.
func carriedByPi(rec Record, res wire.Result, engine string) bool {
	return engine == agentid.Pi && rec.Agent != agentid.Pi && carriedFor(res, rec.Agent)
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

// CodemodeEnv is CODEMODE plus the agent's vendor switch for a code-mode run; nil when off or the agent has no var row.
func CodemodeEnv(name, mode string) []string {
	row := agentid.Routing(name).CodeMode
	if !rtkwrap.CodemodeOn(mode) || row.Var == "" {
		return nil
	}
	env := []string{rtkwrap.CodemodeEnv + "=on"}
	if !row.ViaSettings { // the vendor reads that one from --settings, never the process env
		env = append(env, row.Var+"=on")
	}
	return env
}

// ownCLICodemodeOff: name is a roster agent whose own CLI has no code-mode row.
func ownCLICodemodeOff(name string) bool {
	return agentid.Known(name) && !agentid.Routing(name).CodeMode.Has()
}

// ownCLIReason is why an own-CLI hop runs code mode off.
const ownCLIReason = "own CLI has no code mode; it runs on the pi runner hop only"

// hopCodemode is req for one hop: an own CLI with no code-mode row runs it off, the shared record untouched.
func hopCodemode(name string, req RunOptions) RunOptions {
	if rtkwrap.CodemodeOn(req.Codemode) && ownCLICodemodeOff(name) {
		return withoutCodemode(name, ownCLIReason, req)
	}
	return req
}

// withCodemodeEnv layers the code-mode env after --env, so the typed flag wins; tools off means no scripts.
func withCodemodeEnv(name string, req RunOptions) RunOptions {
	req = hopCodemode(name, req)
	if req.Tools {
		req.Env = slices.Concat(req.Env, CodemodeEnv(name, req.Codemode))
	}
	return req
}

// CodemodeOff is req with code mode off at launch: noted on stderr and in the running record, CODEMODE dropped.
func CodemodeOff(ctx context.Context, name, reason string, req RunOptions) RunOptions {
	fmt.Fprintf(os.Stderr, "codemode: off for %s (%s)\n", name, reason)
	if id := DispatchIDOf(ctx); id != "" && !Peeking(ctx) {
		_, _, _ = updateRecord(id, func(r Record) (Record, bool) {
			r.Request.Codemode, r.Request.CodemodeReason = "off", reason
			return r, true
		})
	}
	return withoutCodemode(name, reason, req)
}

// withoutCodemode is req with code mode off and its reason set, CODEMODE and the vendor switch dropped from the env.
func withoutCodemode(name, reason string, req RunOptions) RunOptions {
	req.Codemode, req.CodemodeReason = "off", reason
	vendor := agentid.Routing(name).CodeMode.Var
	req.Env = slices.DeleteFunc(slices.Clone(req.Env), func(kv string) bool {
		return strings.HasPrefix(kv, rtkwrap.CodemodeEnv+"=") || (vendor != "" && strings.HasPrefix(kv, vendor+"="))
	})
	return req
}
