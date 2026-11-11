package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Dispatch is one routed step: the agent plus the cell it runs (model, effort) and the account it bills.
type Dispatch struct {
	Agent                        Engine
	Model, Effort                string
	Account, Billing             string
	note                         string
	repick                       func(failed string) string // discovery: skip the failed model, return the next pool model or ""
	servedRepick                 func(failed string) string // codex on Databricks: mark the endpoint gone, return the next
	keepCaller, unroutedDispatch bool                       // keepCaller: the wanted dispatch keeps the caller's own --model
	otherRoute                   bool                       // not the wanted agent's own login or pinned profile
	rung                         int                        // 1-based index of the pick candidate that built this dispatch; 0 = none
}

// Chain is a routed dispatch: Want is the agent asked for, Dispatches run in order, Skipped are gate drops.
type Chain struct {
	Want       string
	Dispatches []Dispatch
	Skipped    []string
}

// Single is a one-dispatch chain on r with the caller's request untouched (an explicit backend, or a test).
func Single(r Engine) Chain {
	return Chain{Want: r.Name(), Dispatches: []Dispatch{{Agent: r, keepCaller: true, unroutedDispatch: true}}}
}

// State seams: tests point them away from the real quota.json.
var (
	loadState     = router.LoadState
	markExhausted = router.MarkExhausted
	markAuth      = router.MarkAuth
)

// SwapLoadState replaces the router state loader with wrap(prev) and returns the restore func (tests only).
func SwapLoadState(wrap func(prev func() router.State) func() router.State) func() {
	prev := loadState
	loadState = wrap(prev)
	return func() { loadState = prev }
}

// modelRow resolves a --model (or alias) value to the route-table row it names: the alias the pick runs at and the
// canonical Claude id of the row. exact is the canonical id of a non-Claude model, which only agents serving it may run.
func modelRow(model string) (alias, row, exact string) {
	if model == router.DiscoveryModel {
		return "", "", ""
	}
	canonical, _, ok := models.Resolve(model)
	switch {
	case !ok:
		return "", "", ""
	case models.FamilyKind(canonical) != models.FamilyClaude:
		return "", "", canonical
	}
	alias = cmp.Or(models.AliasOf(canonical), models.FamilyOf(canonical))
	if !slices.Contains(router.Aliases, alias) {
		alias = ""
	}
	return alias, canonical, ""
}

// RouteChainFor routes a caller's --model: an alias or Claude id picks that row's cell; a non-Claude id is exact.
func RouteChainFor(ctx context.Context, want, model, aliasHint, mode string, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	alias, row, exact := modelRow(model)
	return routeChain(ctx, want, cmp.Or(alias, aliasHint), row, exact, strings.TrimSpace(model), mode, strings.TrimSpace(model) != "", native, o)
}

func routeChain(ctx context.Context, want, alias, row, exact, callerModel, mode string, explicitModel, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	o, pins, err := routePickOpts(o, mode)
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	// --native opencode runs on opencode's own config first: no hub login gate, no table cell
	pickWant, nativeLead := want, native && IsHubAgent(want)
	if nativeLead {
		pickWant = ""
	}
	pick := tb.Pick(ctx, alias, pickWant, loadState(), o)
	reg := loadRegistryWarn()
	c := Chain{Want: want}
	if nativeLead {
		c.Dispatches = append(c.Dispatches, Dispatch{Agent: newCLIEngine("opencode"), keepCaller: true, unroutedDispatch: true})
		if mode == router.FallbackNone {
			return c, pick, nil
		}
	}
	c.Skipped = pickSkips(pick, nativeLead)
	current, _ := models.Alias(alias)
	rc := routeCtx{tb: tb, reg: reg, pins: pins, want: want, lead: o.Lead, row: row, exact: exact, callerModel: callerModel,
		current: current, dispatchID: nonClaudeDispatchID(exact, explicitModel, pins[want].Model),
		explicitModel: explicitModel, native: native}
	c, err = rc.addCandidates(c, pick, nativeLead, alias)
	if err != nil {
		return Chain{}, pick, err
	}
	return c, pick, nil
}

var registryWarnOnce sync.Once

// loadRegistryWarn loads the provider registry; on failure it warns once per process that registry rows are dropped.
func loadRegistryWarn() *providers.Registry {
	reg, err := providers.Load()
	if err != nil {
		registryWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: provider registry failed to load, registry-dependent routes are dropped: %v\n", err)
		})
	}
	return reg
}

// routePickOpts fills the fallback mode, the account resolver and the roster in o, and loads the pins.
func routePickOpts(o router.PickOpts, mode string) (router.PickOpts, map[string]router.PinSpec, error) {
	o.Fallback = mode
	if o.Account == nil {
		o.Account = ResolveAccount
	}
	o, err := RosterPick(o)
	if err != nil {
		return o, nil, err
	}
	return o, o.Pins, nil
}

// pickSkips lists the gate drops as "agent: reason", leaving out hub agents when opencode leads natively.
func pickSkips(pick router.PickResult, nativeLead bool) []string {
	var skipped []string
	for _, s := range pick.Skipped {
		if !nativeLead || !IsHubAgent(s.Agent) {
			skipped = append(skipped, s.Agent+": "+s.Reason)
		}
	}
	return skipped
}

// addCandidates appends a dispatch to c for each pick candidate whose cell can run, recording the rest as skipped.
func (rc routeCtx) addCandidates(c Chain, pick router.PickResult, nativeLead bool, alias string) (Chain, error) {
	for i, cand := range pick.Candidates {
		if nativeLead && IsHubAgent(cand.Agent) {
			continue
		}
		cell, skip := rc.candidateCell(cand)
		if skip != "" {
			c.Skipped = append(c.Skipped, skip)
			continue
		}
		disp, skip, err := rc.buildDispatch(cand, cell)
		if err != nil {
			return Chain{}, err
		}
		if skip != "" {
			c.Skipped = append(c.Skipped, skip)
			continue
		}
		disp = rc.discoveryCandidate(disp, cand, alias)
		disp.servedRepick = rc.servedRepickFor(cand)
		disp.otherRoute, disp.rung = rc.otherRoute(cand), i+1
		if !slices.ContainsFunc(c.Dispatches, disp.same) { // ladder rung models can collapse onto one cell
			c.Dispatches = append(c.Dispatches, disp)
		}
	}
	return c, nil
}

// otherRoute reports a candidate that is not the wanted agent's own login or pinned profile.
func (rc routeCtx) otherRoute(cand router.Candidate) bool {
	if rc.lead != "" && cand.Agent == rc.want {
		return cand.Profile != rc.lead
	}
	return cand.Agent != rc.want || (rc.want != "claude" && cand.Profile != rc.pins[rc.want].Profile)
}

// same reports whether o runs the same agent, account, model and effort as d.
func (d Dispatch) same(o Dispatch) bool {
	return d.Agent.Name() == o.Agent.Name() && d.Account == o.Account && d.Model == o.Model && d.Effort == o.Effort
}

// explicitChain is r alone on the caller's explicit backend (--provider or provider:model), carrying that backend's
// account: a remembered exhaustion skips it without an attempt, and a quota failure is recorded on it.
func explicitChain(r Engine, req RunOptions) Chain {
	c := Single(r)
	acct, billing := explicitAccount(r, req)
	c.Dispatches[0].Account, c.Dispatches[0].Billing = acct, billing
	if req.Model == router.DiscoveryModel {
		reg := loadRegistryWarn()
		c.Dispatches[0] = discoveryDispatch(c.Dispatches[0], router.Aliases[1], reg, req.Provider, false)
	}
	if acct == "" || acct == copilotLoginAccount() { // copilot's own gate falls back to Databricks instead
		return c
	}
	now := time.Now()
	if e := loadState()[acct]; e.Status(now) == router.StateExhausted {
		return Chain{Want: r.Name(), Skipped: []string{r.Name() + ": " + router.ExhaustedReason(acct, e, now)}}
	}
	return c
}

// withAlias runs c's lone explicit dispatch on the router's alias when the caller named no model.
func withAlias(c Chain, alias string) Chain {
	if len(c.Dispatches) != 1 || alias == "" {
		return c
	}
	disp := c.Dispatches[0]
	disp.Model, disp.keepCaller, disp.unroutedDispatch = alias, false, false
	c.Dispatches = []Dispatch{disp}
	return c
}

// dispatchRequest is req for dispatch: the wanted dispatch keeps an explicit caller model; every other dispatch drops the original
// provider and session (they belong to the dispatch that minted them) and runs its cell, the caller's --effort winning.
func dispatchRequest(req RunOptions, disp Dispatch, first bool) RunOptions {
	disp.discoveryNote()
	out := req
	if disp.unroutedDispatch {
		return out
	}
	if !first {
		out.Provider, out.SessionID = "", ""
	}
	if !disp.keepCaller {
		out.Model = disp.Model
	}
	if out.Effort == "" {
		out.Effort = disp.Effort
	}
	return armClaudeProfile(out, disp)
}

// armClaudeProfile runs a claude hop inside databricks:<p> on its "<profile>:" row; claude reads only Provider.
func armClaudeProfile(out RunOptions, disp Dispatch) RunOptions {
	if router.ArmProfile() == "" || disp.Agent == nil || disp.Agent.Name() != "claude" {
		return out
	}
	prof, model, ok := strings.Cut(out.Model, ":")
	if reg, err := providers.Load(); ok && err == nil && reg.Providers[prof] != nil {
		out.Provider, out.Model = prof, model
	}
	return out
}

// RouteReq is one route ask: the agent, its model, the caller's route flags and what the chain is for.
type RouteReq struct {
	Agent, Model, AliasHint, Task string // AliasHint: the dispatch's classified alias; Task: what a preview classifies
	Profile, Provider, Lead       string // --profile (a name or LeadSession), --provider, an already-resolved lead
	Fallback, WorkDir             string
	Native, Preview               bool
	Pinned                        bool   // Profile is an explicit backend whatever the fallback (a dispatch's pin)
	Host                          bool   // Engine runs remotely as one unrouted dispatch (--host)
	Engine                        Engine // the resolved engine an explicit or --host chain runs on
	Refresh                       func(context.Context, string) bool
}

// Resolve is the one route entry: lead (flags, session), explicit backend, routed ladder, or a no-run preview.
func Resolve(ctx context.Context, q RouteReq) (Chain, router.PickResult, error) {
	if q.Host {
		return Single(q.Engine), router.PickResult{}, nil
	}
	lead, explicit, err := routeLead(q)
	if err != nil {
		return Chain{}, router.PickResult{}, &LeadError{err}
	}
	if explicit {
		c, err := resolveExplicit(q)
		return c, router.PickResult{}, err
	}
	if q.Preview {
		return previewRoute(ctx, q, lead)
	}
	if err := ctx.Err(); err != nil {
		return Chain{}, router.PickResult{}, err
	}
	return chainFor(ctx, q, q.AliasHint, lead)
}

// LeadError is a --profile/--provider (or pin) that cannot lead; callers map it to a usage exit.
type LeadError struct{ Err error }

func (e *LeadError) Error() string { return e.Err.Error() }
func (e *LeadError) Unwrap() error { return e.Err }

// chainFor builds the routed chain for q with the given alias hint and lead.
func chainFor(ctx context.Context, q RouteReq, hint, lead string) (Chain, router.PickResult, error) {
	return RouteChainFor(ctx, q.Agent, q.Model, hint, q.Fallback, q.Native,
		router.PickOpts{Lead: lead, Refresh: q.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(q.WorkDir) }})
}

// routeLead is LeadFor, except a preview leads on its pin or the profile of a profile:model, never runs it explicitly.
func routeLead(q RouteReq) (lead string, explicit bool, err error) {
	lead, explicit, err = LeadFor(q)
	if err != nil || !explicit || !q.Preview {
		return lead, explicit, err
	}
	if q.Profile != "" {
		return q.Profile, false, nil
	}
	if name, _, ok := strings.Cut(q.Model, ":"); ok {
		return name, false, nil
	}
	return "", false, nil
}

// LeadFor is the one pin-or-lead predicate: a named --profile pins (explicit) only with --fallback none or Pinned.
func LeadFor(q RouteReq) (lead string, explicit bool, err error) {
	if q.Lead != "" {
		return q.Lead, false, nil
	}
	if q.Profile == "" && q.Provider == "" {
		return "", ExplicitBackend(RunOptions{Model: q.Model}), nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", false, err
	}
	if q.Provider == "" && q.Profile != LeadSession && (q.Pinned || q.Fallback == router.FallbackNone) {
		_, err := reg.Get(q.Profile) // an unknown pin errs with explicit set: dispatch reports it, a lead check skips it
		return "", true, err
	}
	cfg, err := agentconfig.For(q.Agent, agentconfig.Launch{Profile: q.Profile, Provider: q.Provider, Routed: true, CanServe: CanServe})
	return cfg.Profile, false, err
}

// resolveExplicit builds a named backend's chain (unknown errs); a hub agent on a hub profile runs a hub dispatch.
func resolveExplicit(q RouteReq) (Chain, error) {
	r := q.Engine
	if q.Profile != "" {
		reg, err := providers.Load()
		if err != nil {
			return Chain{}, err
		}
		p, err := reg.Get(q.Profile)
		if err != nil {
			return Chain{}, err
		}
		if !q.Native && IsHubAgent(q.Agent) && p.OpencodeID != "" {
			start, err := HubStart(reg, q.Agent, q.Profile)
			if err != nil {
				return Chain{}, err
			}
			r = NewHubDispatch(q.Agent, start, reg)
		}
	}
	chain := explicitChain(r, RunOptions{Provider: q.Profile, Model: q.Model})
	if IsHubAgent(q.Agent) && q.Model == "" {
		chain = withAlias(chain, q.AliasHint)
	}
	return chain, nil
}

// LeadSession is the --profile value naming the session's own profile (CLAUDE_PROFILE, else claude's record).
const LeadSession = "session"

// firstOutputDeadline is the free-dispatch stall deadline from the registry, else OpencodeRouting's 90s floor.
func firstOutputDeadline() time.Duration {
	reg, err := providers.Load()
	if err != nil {
		return (providers.OpencodeRouting{}).FirstOutput()
	}
	return reg.Routing.Opencode.FirstOutput()
}

// ErrStalled is the sentinel a dispatch's Run returns when its stream went silent past the stall deadline: a
// fatal, unwrapped failure, so Do never retries it but dispatchUnavailable falls through to the chain's backup dispatch.
var ErrStalled = errors.New("dispatch stalled: no output within the stall deadline")

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

func (rc routeCtx) discoveryCandidate(disp Dispatch, cand router.Candidate, alias string) Dispatch {
	if cand.Agent != rc.want {
		return disp
	}
	pin := rc.pins[rc.want]
	if rc.callerModel != "" && rc.callerModel != router.DiscoveryModel {
		return disp
	}
	if rc.callerModel == "" && pin.Model != router.DiscoveryModel {
		return disp
	}
	picked := cand.Profile != "" && pin.Profile == cand.Profile && pin.Model == router.DiscoveryModel
	return discoveryDispatch(disp, alias, rc.reg, cand.Profile, picked)
}

// discoveryDispatch freezes a pool choice before an agent or agent resolver sees the request.
func discoveryDispatch(disp Dispatch, alias string, reg *providers.Registry, profile string, picked bool) Dispatch {
	tb, err := router.Load()
	if err != nil {
		disp.Model, disp.keepCaller, disp.unroutedDispatch = alias, false, false
		disp.note = "discovery routing table unavailable; using alias"
		return disp
	}
	cell, _ := tb.Cell(disp.Agent.Name(), alias)
	p, prefix := discoveryProvider(disp, reg, profile)
	if p != nil && p.Preset == providers.PresetDatabricks {
		disp.repick = discoveryRepick(disp.Agent.Name(), alias, cell, p, prefix)
	}
	if picked && router.IsPoolModel(p, disp.Model) {
		return disp
	}
	model, note := router.PinnedModel(disp.Agent.Name(), alias, cell, p, router.DiscoveryModel)
	disp.Model, disp.note, disp.keepCaller, disp.unroutedDispatch = prefix+model, note, false, false
	return disp
}

func discoveryProvider(disp Dispatch, reg *providers.Registry, profile string) (*providers.Provider, string) {
	if h, ok := disp.Agent.(HubEngine); ok {
		return h.HubProvider(), ""
	}
	if profile == "" {
		_, profile, _ = strings.Cut(disp.Account, "·")
	}
	p := reg.Row(profile)
	if p == nil {
		return nil, ""
	}
	return p, p.Name + ":"
}

// discoveryRepick skips the failed model for this agent, then returns the next pool pick ("" if unchanged).
func discoveryRepick(agent, alias string, cell router.Cell, p *providers.Provider, prefix string) func(string) string {
	return func(failed string) string {
		wire := strings.TrimPrefix(failed, prefix)
		if router.SkipDiscovery(agent, p, wire) == "" {
			return ""
		}
		next, _ := router.PinnedModel(agent, alias, cell, p, router.DiscoveryModel)
		if next == "" || next == wire {
			return ""
		}
		fmt.Fprintf(os.Stderr, "discovery: %s skipped for %s (model capability)\n", wire, agent)
		return prefix + next
	}
}

// capabilityErr is a model refusing the request itself: an output cap, or Google rejecting JSON-schema tool keys.
var capabilityErr = regexp.MustCompile(`max_new_tokens|max_output_tokens|max_tokens|context length|maximum context|Invalid JSON payload received\. Unknown name`)

// endpointErr is a workspace's answer for a serving endpoint that does not exist (captured from codex on Databricks).
var endpointErr = regexp.MustCompile(`ENDPOINT_NOT_FOUND`)

// servedRepickFor is a codex Databricks dispatch's repick when the switch is on and the caller named no exact model.
func (rc routeCtx) servedRepickFor(cand router.Candidate) func(string) string {
	p := rc.codexDatabricks(cand)
	if p == nil || !providers.ServedRoutingOn() || rc.exact != "" || (rc.callerModel != "" && rc.row == "") {
		return nil
	}
	return func(failed string) string { return servedRepick(p, failed) }
}

// codexDatabricks is cand's profile row when a Responses-wire agent dispatches on a Databricks profile, else nil.
func (rc routeCtx) codexDatabricks(cand router.Candidate) *providers.Provider {
	if !speaksResponses(cand.Agent) || cand.Profile == "" || rc.reg == nil {
		return nil
	}
	if p := rc.reg.Providers[cand.Profile]; p != nil && p.Preset == providers.PresetDatabricks {
		return p
	}
	return nil
}

// servedRepick marks the failed model gone on p and returns the next READY Responses model, "" when there is none.
func servedRepick(p *providers.Provider, failed string) string {
	prefix := p.Name + ":"
	wire := strings.TrimPrefix(failed, prefix)
	if err := router.MarkUnserved(p, wire); err != nil {
		fmt.Fprintf(os.Stderr, "served: could not mark %s unavailable: %v\n", wire, err)
	}
	next, _ := router.ServedFirst(p, true, time.Now())
	if next == "" || next == wire {
		return ""
	}
	fmt.Fprintf(os.Stderr, "served: %s unavailable on %s; using %s\n", wire, p.Name, next)
	return prefix + next
}

// retryModel is the next model after a model-capability (discovery) or endpoint-gone (served) failure, else "".
func retryModel(disp Dispatch, lreq RunOptions, err error) string {
	if err == nil {
		return ""
	}
	if retry.Progressed(err) {
		return "" // text or tools already ran: a re-run on the next model would repeat them
	}
	switch msg := err.Error(); {
	case disp.repick != nil && capabilityErr.MatchString(msg):
		return disp.repick(lreq.Model)
	case disp.servedRepick != nil && endpointErr.MatchString(msg):
		return disp.servedRepick(lreq.Model)
	}
	return ""
}

func (disp Dispatch) discoveryNote() {
	if disp.note == "" {
		return
	}
	fmt.Fprintln(os.Stderr, disp.note)
}
