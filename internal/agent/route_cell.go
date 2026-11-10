package agent

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// routeCtx is the per-pick state routeChain's candidate helpers share.
type routeCtx struct {
	tb                            *router.RouteTable
	reg                           *providers.Registry
	pins                          map[string]router.PinSpec
	want, row, exact, callerModel string
	lead                          string // profile the wanted agent leads with; "" = its pin
	current, dispatchID           string
	explicitModel, native         bool
}

// candidateCell adjusts cand's cell for the row or exact model; skip is non-empty when cand serves none.
func (rc routeCtx) candidateCell(cand router.Candidate) (cell router.Cell, skip string) {
	cell = cand.Cell
	switch {
	case cand.Agent == "claude" && rc.dispatchID != "": // lowest Claude alias serving the non-Claude dispatch
		rc.claudeTierCell(&cell)
	case rc.pinnedCaller(cand):
		rc.pinnedCell(cand, &cell)
	case rc.callerOnProfile(cand): // the wanted agent's profile row runs the caller's --model, never its pin or tier
		return rc.callerProfileCell(cand, cell)
	case rc.profileBacked(cand): // a Databricks dispatch serves only its own cell's model
		return rc.profileCell(cand, cell)
	case rc.exact != "": // a non-Claude id runs only where a cell serves it
		return rc.exactCell(cand, cell)
	case rc.olderRow(cand): // an older row runs its own cell
		if oc, ok := rc.tb.ByModel[rc.row][cand.Agent]; ok {
			cell.Model, cell.Effort = oc.Model, oc.Effort
		}
	}
	return cell, ""
}

// pinnedCaller reports the wanted agent on its pinned profile running the caller's own non-row model.
func (rc routeCtx) pinnedCaller(cand router.Candidate) bool {
	pinned := cand.Profile != "" && rc.pins[cand.Agent].Profile == cand.Profile
	return pinned && cand.Agent == rc.want && rc.explicitModel && rc.callerModel != router.DiscoveryModel &&
		rc.row == "" && cmp.Or(rc.exact, rc.callerModel) != ""
}

// pinnedCell runs the caller's model on cand's pinned profile, respelled for it.
func (rc routeCtx) pinnedCell(cand router.Candidate, cell *router.Cell) {
	cell.Model = cmp.Or(rc.exact, strings.TrimPrefix(rc.callerModel, cand.Profile+":")) // buildDispatch re-adds the profile
	if rc.reg != nil && rc.reg.Providers[cand.Profile] != nil {
		cell.Model = rc.reg.Providers[cand.Profile].WireModel(cell.Model)
	}
}

// callerOnProfile reports the wanted agent on a registry profile asked for a concrete id (an alias keeps the tier).
func (rc routeCtx) callerOnProfile(cand router.Candidate) bool {
	concrete := rc.explicitModel && cmp.Or(rc.exact, rc.row) != "" && !slices.Contains(models.AliasNames, rc.callerModel)
	known := cand.Profile != "" && rc.reg != nil && rc.reg.Providers[cand.Profile] != nil
	return concrete && known && cand.Agent == rc.want && wireAgent(cand.Agent)
}

// wireAgent is an agent with dialects whose profile row takes a caller's model: not claude, a hub or codex.
func wireAgent(name string) bool {
	s, ok := LookupSchema(name)
	return ok && len(s.CLI.Dialects) > 0 && name != "claude" && !IsHubAgent(name) && !speaksResponses(name)
}

// callerProfileCell runs the caller's model as cand's profile serves it, or skips the row naming why it cannot.
func (rc routeCtx) callerProfileCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	model := strings.TrimPrefix(rc.callerModel, cand.Profile+":")
	wire, why := ProfileServes(cand.Agent, rc.reg.Providers[cand.Profile], model)
	if why != "" {
		return cell, cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + "): " + why
	}
	cell.Model = wire
	return cell, ""
}

// ProfileServes is model as p spells it when a fresh listing does not mark it NOT_READY and agentName Serves it there.
func ProfileServes(agentName string, p *providers.Provider, model string) (wire, why string) {
	if wire = providers.SpellPin(p, model); wire == "" {
		return "", "serves no " + model
	}
	if ready, fresh := router.ServedReady(p, wire, time.Now()); fresh && !ready {
		return "", "serves no " + wire + " (not a READY endpoint on " + p.Name + ")"
	}
	if ok, why := Serves(agentName, p, wire); !ok {
		return "", why
	}
	return wire, ""
}

// profileBacked reports a non-hub profile candidate facing an exact non-Claude model.
func (rc routeCtx) profileBacked(cand router.Candidate) bool {
	return rc.exact != "" && cand.Profile != "" && !IsHubAgent(cand.Agent)
}

// olderRow reports an older-row request that cand's own login can serve.
func (rc routeCtx) olderRow(cand router.Candidate) bool {
	return rc.row != "" && rc.row != rc.current && !IsHubAgent(cand.Agent) && cand.Profile == ""
}

// exactCell points cell at the cell serving the exact model; skip is non-empty for a non-wanted agent with none.
func (rc routeCtx) exactCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	sc, ok := cellServing(rc.tb, cand.Agent, rc.exact)
	if !ok && cand.Agent != rc.want {
		return cell, cand.Agent + ": serves no " + rc.exact
	}
	if ok {
		cell.Model, cell.Effort = sc.Model, sc.Effort
	}
	return cell, ""
}

// speaksResponses reports an agent whose schema lists the Responses dialect (codex): it needs a Responses route.
func speaksResponses(agentName string) bool {
	s, ok := LookupSchema(agentName)
	return ok && slices.Contains(s.CLI.Dialects, providers.ProtocolResponses)
}

// profileCell validates a profile-backed candidate against the exact model: codex needs a Responses route for it,
// any other agent a cell that already serves it; the accepted cell runs the model as the profile spells it.
func (rc routeCtx) profileCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	who := cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + ")"
	var p *providers.Provider
	if rc.reg != nil {
		p = rc.reg.Providers[cand.Profile]
	}
	if speaksResponses(cand.Agent) && p != nil {
		wire := p.WireModel(rc.exact)
		if !providers.ServesResponses(p, wire) {
			return cell, who + ": serves no " + rc.exact + " over Responses"
		}
		if ready, fresh := router.ServedReady(p, wire, time.Now()); fresh && !ready {
			return cell, who + ": " + rc.exact + " is not a READY endpoint on " + cand.Profile
		}
		cell.Model = wire
		return cell, ""
	}
	if id, _, ok := models.Resolve(cell.Model); !ok || id != rc.exact {
		return cell, who + ": serves no " + rc.exact
	}
	return cell, ""
}

// claudeTierCell points cell at claude's lowest alias serving the non-Claude dispatch model.
func (rc routeCtx) claudeTierCell(cell *router.Cell) {
	t := lowestAliasServing(rc.tb, rc.want, rc.dispatchID)
	if t == "" {
		return
	}
	if tc, ok := rc.tb.Rows["claude"][t]; ok {
		cell.Model, cell.Effort = tc.Model, tc.Effort
	}
}

// buildDispatch turns cand's cell into a dispatch; skip is non-empty when the dispatch cannot run, err when its agent is unknown.
func (rc routeCtx) buildDispatch(cand router.Candidate, cell router.Cell) (disp Dispatch, skip string, err error) {
	disp = Dispatch{Model: cell.Model, Effort: cell.Effort, Account: cand.Account, Billing: cand.Billing}
	disp.keepCaller = cand.Agent == rc.want && rc.explicitModel && cand.Profile == "" && rc.callerModel != router.DiscoveryModel
	if disp.keepCaller && rc.exact != "" {
		disp.Model = rc.exact // an exact non-Claude id runs as asked; an alias runs the cell's model
	}
	if cand.Profile != "" { // provider:model pins the dispatch to that Databricks profile, not the session's
		disp.Model = cand.Profile + ":" + cell.Model
	}
	return rc.dispatchAgent(cand, cell, disp)
}

// hubOpencode reports a candidate that rides the opencode hub rather than its own agent CLI.
func (rc routeCtx) hubOpencode(cand router.Candidate) bool {
	if pr := rc.reg.Row(cand.Profile); pr != nil && pr.OpencodeID == "" {
		return false // a row with no opencode id can't ride the hub; the native agent takes <profile>:<model>
	}
	return IsHubAgent(cand.Agent) && (!rc.native || cand.Agent != rc.want)
}

// dispatchAgent sets disp's agent (and model) by how cand runs: a hub dispatch or its own agent.
func (rc routeCtx) dispatchAgent(cand router.Candidate, cell router.Cell, disp Dispatch) (Dispatch, string, error) {
	hub := rc.hubOpencode(cand)
	switch {
	case hub && hubLinked() && cand.Profile != "" && rc.reg != nil && rc.reg.Providers[cand.Profile] != nil:
		// a pinned registry row runs the hub dispatch on that profile, bare model
		_, bare, ok := strings.Cut(cell.Model, "/")
		if !ok {
			bare = cell.Model
		}
		disp.Agent, disp.Model = watchFreeDispatch(NewHubDispatch("opencode", rc.reg.Providers[cand.Profile], rc.reg), cand.Billing), bare
	case hub:
		r, model, err := hubDispatchFor(rc.reg, cell.Model)
		if err != nil {
			return disp, "opencode: " + err.Error(), nil
		}
		disp.Agent, disp.Model = watchFreeDispatch(r, cand.Billing), model
	default:
		var err error
		disp, err = lookupDispatch(cand, cell, disp)
		return disp, "", err
	}
	return disp, "", nil
}

// lookupDispatch sets disp's agent to cand's own engine; a claude dispatch runs the cell's alias.
func lookupDispatch(cand router.Candidate, cell router.Cell, disp Dispatch) (Dispatch, error) {
	r, err := Lookup(cand.Agent)
	if err != nil {
		return disp, err
	}
	disp.Agent = r
	if cand.Agent == "claude" {
		disp.Model = claudeCellModel(cell.Model)
		if cand.Profile != "" { // keep the <profile>: spelling so the dispatch resolves on that profile
			disp.Model = cand.Profile + ":" + disp.Model
		}
	}
	return disp, nil
}

// nonClaudeDispatchID is the canonical non-Claude model the dispatch runs: the exact --model, else the wanted agent's model pin.
func nonClaudeDispatchID(exact string, explicitModel bool, pinModel string) string {
	if exact != "" || explicitModel || pinModel == "" {
		return exact
	}
	if id, _, ok := models.Resolve(pinModel); ok && models.FamilyKind(id) != models.FamilyClaude {
		return id
	}
	return ""
}

// lowestAliasServing is the lowest router alias (haiku up) whose cell for agentName resolves to id, else "".
// Several aliases may share id (codex gpt-6.1-sol at opus and sonnet); a caller that named an alias never reaches this.
func lowestAliasServing(tb *router.RouteTable, agentName, id string) string {
	for _, alias := range slices.Backward(router.Aliases) {
		if c, ok := tb.Rows[agentName][alias]; ok {
			if cid, _, ok := models.Resolve(c.Model); ok && cid == id {
				return alias
			}
		}
	}
	return ""
}

// claudeCellModel is the model a routed claude dispatch runs: the alias a canonical row id backs, so the session's
// selection (CLAUDE_<TIER>_MODEL pins, auto-[1m]) resolves it; an older row without an alias stays its canonical id.
func claudeCellModel(cell string) string {
	canonical, _, ok := models.Resolve(cell)
	if !ok {
		return cell
	}
	return cmp.Or(models.AliasOf(canonical), canonical)
}

// cellServing is agentName's table cell (any row, then opencode's sub-candidates) whose model is exact.
func cellServing(tb *router.RouteTable, agentName, exact string) (router.Cell, bool) {
	serves := func(c router.Cell) bool { id, _, ok := models.Resolve(c.Model); return ok && id == exact }
	for _, row := range slices.Sorted(maps.Keys(tb.ByModel)) {
		if c, ok := tb.ByModel[row][agentName]; ok && serves(c) {
			return c, true
		}
	}
	if !IsHubAgent(agentName) {
		return router.Cell{}, false
	}
	for _, sub := range slices.Sorted(maps.Keys(tb.OpencodeSub)) {
		for _, alias := range slices.Sorted(maps.Keys(tb.OpencodeSub[sub])) {
			if c := tb.OpencodeSub[sub][alias]; serves(c) {
				return c, true
			}
		}
	}
	return router.Cell{}, false
}

// watchFreeDispatch sets the stall watchdog on r when billing is free; any other agent (or billing) passes through.
func watchFreeDispatch(r Engine, billing string) Engine {
	if billing != router.BillFree {
		return r
	}
	if hl, ok := r.(HubEngine); ok {
		return hl.WithFirstOutput(firstOutputDeadline())
	}
	return r
}
