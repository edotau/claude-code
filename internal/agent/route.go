package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Dispatch is one routed step: the agent plus the cell it runs (model, effort) and the account it bills.
type Dispatch struct {
	Agent                        Engine
	Model, Effort                string
	Account, Billing             string
	note                         string
	servedRepick                 func(failed string) string // codex on Databricks: mark the endpoint gone, return the next
	keepCaller, unroutedDispatch bool                       // keepCaller: the wanted dispatch keeps the caller's own --model
	otherRoute                   bool                       // not the wanted agent's own login or pinned profile
	swap                         bool                       // the wanted agent's pin standing in for the requested target
	routerChose                  bool                       // no lead and no pin: the router's own row for the wanted agent, not a deviation
	rung                         int                        // 1-based index of the pick candidate that built this dispatch; 0 = none
	quotaOverflow                bool                       // the last hop: runs only after a quota-class failure
}

// Chain is a routed dispatch: Want is the agent asked for, Dispatches run in order, Skipped are gate drops.
type Chain struct {
	Want       string
	Dispatches []Dispatch
	Skipped    []string
	Preflight  *Preflight // nil when the preflight failed nothing and skipped no check
	requested  string     // the requested target's <profile>:<model> when it is a dispatch of this chain
}

// Hops names each dispatch in run order, "codex (openai·chatgpt, gpt-6.1-sol)".
func (c Chain) Hops() []string {
	out := make([]string, 0, len(c.Dispatches))
	for _, d := range c.Dispatches {
		out = append(out, hopTarget(d))
	}
	return out
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

// RouteChainFor routes a caller's --model: an alias or Claude id picks that row's cell; a non-Claude id is exact.
func RouteChainFor(ctx context.Context, want, model, aliasHint, mode string, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	if err := refuseOwnAuto(want, model); err != nil {
		return Chain{}, router.PickResult{}, err
	}
	if ownConnectorModel(want, model) {
		r, bare, err := hubDispatchFor(loadRegistryWarn(), model)
		if err != nil {
			return Chain{}, router.PickResult{}, err
		}
		acct, bill := explicitAccount(r, RunOptions{}) // the account lets a quota refusal hold the own plan
		d := Dispatch{Agent: watchFirstOutput(r), Model: bare, Account: acct, Billing: bill, keepCaller: true}
		return Chain{Want: want, Dispatches: []Dispatch{d}}, router.PickResult{}, nil
	}
	alias, row, exact := router.ModelRow(model)
	return routeChain(ctx, want, cmp.Or(alias, aliasHint), row, exact, strings.TrimSpace(model), mode, strings.TrimSpace(model) != "", native, o)
}

// ownConnectorQuery is an own-plan model with no explicit backend: an explicit one routes (and is refused) as before.
func ownConnectorQuery(q ModelQuery) bool {
	return q.Profile == "" && q.Provider == "" && ownConnectorModel(q.Agent, q.Model)
}

// refuseOwnAuto refuses `<login>/auto` on an own plan: the vendor has no such id, so it names the login's typed ids.
func refuseOwnAuto(agent, model string) error {
	id, bare, _ := strings.Cut(model, "/")
	if bare != "auto" || !ownConnectorModel(agent, model) {
		return nil
	}
	tb, err := router.Load()
	if err != nil {
		return err
	}
	var ids []string
	for _, cell := range tb.OpencodeSub[id] {
		if !slices.Contains(ids, cell.Model) {
			ids = append(ids, cell.Model)
		}
	}
	slices.Sort(ids)
	return clientUsageError{fmt.Errorf("--model %s: auto is no %s model; use one of %s", model, id, strings.Join(ids, ", "))}
}

// ownConnectorRoute is the route a run on opencode's own plan reports: no registry row, the plan id as its account.
func ownConnectorRoute(q ModelQuery) ModelRoute {
	id, _, _ := strings.Cut(q.Model, "/")
	return ModelRoute{Agent: q.Agent, Account: id, Model: q.Model, Dialect: "own-login", Source: "own-login"}
}

func ownConnectorModel(agent, model string) bool {
	id, _, ok := strings.Cut(model, "/")
	return agentid.Routing(agent).SubLadder && ok && hub != nil && hub.OwnPlan(id)
}

func routeChain(ctx context.Context, want, alias, row, exact, callerModel, mode string, explicitModel, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	o, pins, err := routePickOpts(ctx, o, mode)
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	// --native opencode runs on opencode's own config first: no hub login gate, no table cell
	pickWant, nativeLead := want, native && isHubAgent(want)
	if nativeLead {
		pickWant = ""
	}
	pick := tb.Pick(ctx, alias, pickWant, loadState(), o)
	pick.Skipped = slices.DeleteFunc(pick.Skipped, func(s router.Skip) bool { return router.OffOrder(s.Agent, want) })
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
	family := providers.FamilyName(callerModel)
	rc := routeCtx{ctx: ctx, tb: tb, reg: reg, pins: pins, want: want, lead: o.Lead, row: row, exact: exact, callerModel: callerModel,
		current: current, dispatchID: router.NonClaudeDispatchID(cmp.Or(exact, providers.FamilyDefaults[family]), explicitModel, pins[want].Model),
		family: family, explicitModel: explicitModel, native: native, mode: mode, pf: &Preflight{}}
	c, err = rc.addCandidates(c, pick, nativeLead, alias)
	if err != nil {
		return Chain{}, pick, err
	}
	return rc.finishChain(rc.withAgyOverflow(c, alias, o), pick), pick, nil
}

// withAgyOverflow ends the claude ladder with the Claude model agy serves on the Google login (G3).
func (rc routeCtx) withAgyOverflow(c Chain, alias string, o router.PickOpts) Chain {
	claudeHop := func(d Dispatch) bool { return VaultAgentIsClaude(d.Agent.Name()) }
	if !VaultAgentIsClaude(rc.want) || rc.mode == router.FallbackNone || rc.mode == router.FallbackPin || rc.exact != "" ||
		(rc.row != "" && rc.row != rc.current) || !slices.ContainsFunc(c.Dispatches, claudeHop) {
		return c
	}
	id, _ := models.Alias(alias)
	if len(models.AcceptedEfforts(agentid.Gemini, id)) == 0 { // agy serves no Claude for this alias
		return c
	}
	o.Lead, o.Pins, o.Fallback = "", nil, router.FallbackNone
	cands := rc.tb.Pick(rc.ctx, alias, agentid.Gemini, loadState(), o).Candidates
	i := slices.IndexFunc(cands, func(k router.Candidate) bool { return k.Agent == agentid.Gemini && k.Profile == "" })
	if i < 0 {
		return c
	}
	cell := cands[i].Cell
	cell.Model, cell.Effort = id, ""
	g := rc
	g.want, g.row, g.exact, g.callerModel, g.explicitModel, g.lead = agentid.Gemini, "", "", "", false, ""
	disp, skip, err := g.buildDispatch(cands[i], cell)
	if err != nil || skip != "" {
		return c
	}
	disp.otherRoute, disp.quotaOverflow = true, true
	c.Dispatches = append(c.Dispatches, disp)
	return c
}

// finishChain drops the confinement's skips from a chain that runs and attaches what the preflight found.
func (rc routeCtx) finishChain(c Chain, pick router.PickResult) Chain {
	if len(c.Dispatches) > 0 { // a candidate the confinement set aside was never an attempt; only an empty chain names it
		c.Skipped = slices.DeleteFunc(c.Skipped, func(s string) bool {
			return slices.ContainsFunc(pick.Skipped, func(k router.Skip) bool { return k.Outside() && s == k.Agent+": "+k.Reason })
		})
	}
	if len(c.Dispatches) > 0 && databricksDispatch(rc.reg, c.Dispatches[0]) {
		rc.pf.note(proxyNote())
	}
	if len(rc.pf.Failed)+len(rc.pf.Notes) > 0 {
		if len(c.Dispatches) > 0 {
			rc.pf.Chosen = hopTarget(c.Dispatches[0])
		}
		c.Preflight = rc.pf
	}
	return c
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
func routePickOpts(ctx context.Context, o router.PickOpts, mode string) (router.PickOpts, map[string]router.PinSpec, error) {
	o.Fallback = mode
	if _, ok := proc.ValuesFromContext(ctx); ok && o.Account == nil {
		o.Account = func(name string, cell router.Cell) (string, string) { return resolveAccountContext(ctx, name, cell) }
	}
	if o.Account == nil {
		o.Account = ResolveAccount
	}
	o, err := rosterPick(ctx, o)
	if err != nil {
		return o, nil, err
	}
	return o, o.Pins, nil
}

// pickSkips lists the gate drops as "agent: reason", leaving out hub agents when opencode leads natively.
func pickSkips(pick router.PickResult, nativeLead bool) []string {
	var skipped []string
	for _, s := range pick.Skipped {
		if !nativeLead || !isHubAgent(s.Agent) {
			skipped = append(skipped, s.Agent+": "+s.Reason)
		}
	}
	return skipped
}

// step is one hop of the fallback order: a pick candidate, how its cell is read, and whether it is the pin hop.
type step struct {
	i   int
	rc  routeCtx
	pin bool
}

// steps is the fallback order over the pick: the requested target, the wanted agent's pin, then the last claude hop.
func (rc routeCtx) steps(cands []router.Candidate, alias string, nativeLead bool) (out []step, req int) {
	pin := rc.pinIndex(cands, alias)
	req = router.RequestedIndex(rc.ctx, cands, rc.want, rc.lead, pin)
	if nativeLead { // opencode's own config is the requested target, and its hub pin is no hop of a native run
		req, pin = -1, -1
	}
	if req >= 0 {
		out = append(out, step{i: req, rc: rc})
	}
	if pin >= 0 && !VaultAgentIsClaude(rc.want) && rc.mode != router.FallbackNone && (pin != req || rc.explicitModel) {
		out = append(out, step{i: pin, rc: rc.asPin(), pin: true})
	}
	if fb := rc.recordFallbackIndex(cands); fb >= 0 && fb != req && fb != pin {
		out = append(out, step{i: fb, rc: rc.asPin(), pin: true})
	}
	if last := router.ClaudeBackstop(cands, req, rc.mode); last >= 0 {
		out = append(out, step{i: last, rc: rc})
	}
	return out, req
}

// recordFallbackIndex is the wanted agent's candidate on its record's fallback profile (after the pin); -1 = none.
func (rc routeCtx) recordFallbackIndex(cands []router.Candidate) int {
	fb := vault.GetAgentRecord(rc.want).Fallback
	if fb == "" || rc.mode == router.FallbackNone || VaultAgentIsClaude(rc.want) {
		return -1
	}
	return slices.IndexFunc(cands, func(c router.Candidate) bool { return c.Agent == rc.want && c.Profile == fb })
}

// pinIndex is the pick candidate running the wanted agent on its own pin (profile, else its own login); -1 = none.
func (rc routeCtx) pinIndex(cands []router.Candidate, alias string) int {
	first := func(is func(router.Candidate) bool) int {
		return slices.IndexFunc(cands, func(c router.Candidate) bool { return c.Agent == rc.want && is(c) })
	}
	if rc.pins[rc.want].Profile != "" {
		return first(rc.onPin)
	}
	cell, _ := rc.tb.Cell(rc.want, alias)
	acct, _ := ResolveAccount(rc.want, cell)
	if acct = cmp.Or(acct, cell.Account); acct != "" && !isHubAgent(rc.want) { // several login rows: the signed-in one
		if i := first(func(c router.Candidate) bool { return c.Account == acct }); i >= 0 {
			return i
		}
	}
	return first(func(c router.Candidate) bool { return c.Profile == "" })
}

// onPin reports c on the wanted agent's pinned profile; an opencode sub-pin's candidate has an account, no Profile.
func (rc routeCtx) onPin(c router.Candidate) bool {
	pin := rc.pins[rc.want].Profile
	if pin == "" || c.Profile != "" || !isHubAgent(rc.want) || rc.tb == nil {
		return c.Profile == pin
	}
	for _, cell := range rc.tb.OpencodeSub[pin] {
		if cell.Account != "" && cell.Account == c.Account {
			return true
		}
	}
	return false
}

func (rc routeCtx) asPin() routeCtx {
	rc.lead, rc.row, rc.exact, rc.callerModel, rc.family, rc.explicitModel = "", "", "", "", "", false
	rc.dispatchID = router.NonClaudeDispatchID("", false, rc.pins[rc.want].Model)
	return rc
}

// addCandidates appends a dispatch to c for each step of the fallback order whose cell can run; the rest are skipped.
func (rc routeCtx) addCandidates(c Chain, pick router.PickResult, nativeLead bool, alias string) (Chain, error) {
	steps, req := rc.steps(pick.Candidates, alias, nativeLead)
	armFailed := rc.armFinding()
	for _, s := range steps {
		disp, skip, ok, err := rc.stepDispatch(pick.Candidates[s.i], s, req, armFailed)
		switch {
		case err != nil:
			return Chain{}, err
		case skip != "":
			c.Skipped = append(c.Skipped, skip)
			continue
		case !ok || slices.ContainsFunc(c.Dispatches, disp.same): // the pin is the requested target
			continue
		}
		if s.i == req && !s.pin {
			c.requested = cmp.Or(disp.Model, disp.Account)
		}
		c.Dispatches = append(c.Dispatches, disp)
	}
	return c, nil
}

// stepDispatch is the dispatch for step s on cand, or the reason it is skipped; !ok drops a pin that is the request.
func (rc routeCtx) stepDispatch(cand router.Candidate, s step, req int, armFailed *Finding) (disp Dispatch, skip string, ok bool, err error) {
	if armFailed != nil && s.i == req { // the overlay workspace failed its local check, like a failed model check
		return Dispatch{}, cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + "): preflight " + armFailed.String(), false, nil
	}
	s.rc = s.rc.forFamily(cand)
	cell, skip := s.rc.candidateCell(cand)
	if skip != "" {
		return Dispatch{}, skip, false, nil
	}
	if s.pin && s.i == req && router.AsksModel(rc.callerModel, cand, cell.Model) {
		return Dispatch{}, "", false, nil
	}
	if disp, skip, err = s.rc.buildDispatch(cand, cell); err != nil || skip != "" {
		return Dispatch{}, skip, false, err
	}
	disp.servedRepick = s.rc.servedRepickFor(cand)
	disp.otherRoute, disp.rung, disp.swap = s.rc.otherRoute(cand), s.i+1, s.pin
	disp.routerChose = routerChose(s.rc)
	return disp, "", true, nil
}

// routerChose reports that the caller asked for no route (no lead) and the wanted agent has no pin.
func routerChose(rc routeCtx) bool { return rc.lead == "" && rc.pins[rc.want].Profile == "" }

// otherRoute reports a candidate that is not the wanted agent's own login, pinned profile or overlay workspace.
func (rc routeCtx) otherRoute(cand router.Candidate) bool {
	if router.AskedWorkspace(rc.ctx, cand, rc.want) {
		return false
	}
	if rc.lead != "" && cand.Agent == rc.want {
		return cand.Profile != rc.lead && cand.Account != router.ProviderAccount(rc.lead) // an own-login row has no Profile
	}
	return cand.Agent != rc.want || (!VaultAgentIsClaude(rc.want) && !rc.onPin(cand))
}

func (d Dispatch) same(o Dispatch) bool {
	return d.Agent.Name() == o.Agent.Name() && d.Account == o.Account && d.Model == o.Model && d.Effort == o.Effort
}

// RouteReq is one route ask: the agent, its model, the caller's route flags and what the chain is for.
type RouteReq struct {
	Agent, Model, AliasHint, Task string // AliasHint: the dispatch's classified alias; Task: what a preview classifies
	Profile, Provider, Lead       string // --profile (a name or LeadSession), --provider, an already-resolved lead
	Fallback, WorkDir             string
	Native, Preview               bool
	AutoModel, Tools              bool
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
	lead, explicit, err := routeLeadContext(ctx, q)
	if err != nil {
		return Chain{}, router.PickResult{}, &LeadError{err}
	}
	if explicit {
		c, err := resolveExplicit(ctx, q)
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

func chainFor(ctx context.Context, q RouteReq, hint, lead string) (Chain, router.PickResult, error) {
	return RouteChainFor(ctx, q.Agent, q.Model, hint, q.Fallback, q.Native,
		router.PickOpts{Lead: lead, Refresh: q.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(q.WorkDir) }})
}

// routeLeadContext is LeadFor, except a preview leads on its pin or a profile:model's profile, never explicitly.
func routeLeadContext(ctx context.Context, q RouteReq) (lead string, explicit bool, err error) {
	lead, explicit, err = leadForContext(ctx, q)
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
	return leadForContext(context.Background(), q)
}

func leadForContext(ctx context.Context, q RouteReq) (lead string, explicit bool, err error) {
	if q.Lead != "" {
		return q.Lead, false, nil
	}
	if q.Profile == "" && q.Provider == "" {
		if explicitBackend(RunOptions{Model: q.Model}) {
			return "", true, nil
		}
		return accountOrSessionLead(ctx, q.Agent), false, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", false, err
	}
	if q.Provider == "" && q.Profile != LeadSession && (q.Pinned || q.Fallback == router.FallbackNone) {
		_, err := reg.Get(q.Profile) // an unknown pin errs with explicit set: dispatch reports it, a lead check skips it
		return "", true, err
	}
	cfg, err := agentconfig.ForContext(ctx, q.Agent, agentconfig.Launch{Profile: q.Profile, Provider: q.Provider, Routed: true, CanServe: canLead})
	return cfg.Profile, false, err
}

// canLead is CanServe plus claude, which runs on any Databricks row through the proxy.
func canLead(name string, p *providers.Provider) bool {
	return VaultAgentIsClaude(name) || CanServe(name, p)
}

func accountOrSessionLead(ctx context.Context, agent string) string {
	if account := overlayAccount(ctx); account != "" {
		if reg, err := providers.Load(); err == nil {
			if p := accountRow(reg, account); p != nil {
				return p.Name
			}
		}
	}
	return sessionLead(ctx, agent)
}

func sessionLead(ctx context.Context, agent string) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	if p := sessionRow(reg, QueryFor(ctx, ModelQuery{Agent: agent})); p != nil {
		if ok, _ := rowServes(agent, p); ok {
			return p.Name
		}
	}
	return ""
}

// resolveExplicit builds a named backend's chain (unknown errs); a hub agent on a hub profile runs a hub dispatch.
func resolveExplicit(ctx context.Context, q RouteReq) (Chain, error) {
	if err := explicitRouteErr(ctx, q); err != nil {
		return Chain{}, err
	}
	pf := &Preflight{}
	r, err := explicitEngine(ctx, q, pf)
	if err != nil {
		return Chain{}, err
	}
	chain := explicitChain(r, RunOptions{Provider: q.Profile, Model: q.Model})
	if isHubAgent(q.Agent) && q.Model == "" {
		chain = withAlias(chain, q.AliasHint)
	}
	if len(pf.Notes) > 0 && len(chain.Dispatches) > 0 {
		pf.Chosen, chain.Preflight = hopTarget(chain.Dispatches[0]), pf
	}
	return chain, nil
}

// explicitRouteErr refuses a typed profile or prefix the agent cannot run as written, a usage error (exit 2).
func explicitRouteErr(ctx context.Context, q RouteReq) error {
	reg, err := providers.Load()
	if err != nil || q.Agent == "" || q.Profile == "" && !namesProvider(reg, q.Model) {
		return nil
	}
	mq := QueryFor(PeekContext(ctx), ModelQuery{Agent: q.Agent, Model: q.Model, Profile: q.Profile,
		Task: q.Task, Default: q.AliasHint, Tools: q.Tools})
	_, asked := SplitAuto(q.Model)
	asked = asked || q.AutoModel
	if mq.Model == "" { // an untyped ask is checked against what the profile serves, not the opus alias
		mq.Model = AutoModel
	}
	_, err = ResolveRun(reg, mq, time.Now())
	if errors.Is(err, ErrPoolExhausted) {
		if !asked { // only an asked auto stops on an exhausted pool; an untyped ask keeps its tier pick
			return nil
		}
		return err
	}
	if errors.Is(err, ErrModelRoute) {
		return clientUsageError{err}
	}
	return err
}

// explicitEngine is q's engine after its explicit backend passes preflight; a hub agent on a hub profile is its ladder.
func explicitEngine(ctx context.Context, q RouteReq, pf *Preflight) (Engine, error) {
	if q.Profile == "" {
		if f := modelFinding(ctx, q, pf); f != nil {
			return nil, &PreflightError{Finding: *f}
		}
		return q.Engine, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	p, err := reg.Get(q.Profile)
	if err != nil {
		return nil, err
	}
	if f := explicitFinding(ctx, q, p, pf); f != nil {
		return nil, &PreflightError{Finding: *f}
	}
	if q.Native || !isHubAgent(q.Agent) || p.OpencodeID == "" {
		return q.Engine, nil
	}
	start, err := hubStart(reg, q.Agent, q.Profile)
	if err != nil {
		return nil, err
	}
	return newHubDispatch(q.Agent, start, reg), nil
}

// LeadSession is the --profile value naming the session's own profile (CLAUDE_PROFILE, else claude's record).
const LeadSession = "session"

type routeCtx struct {
	ctx                           context.Context
	tb                            *router.RouteTable
	reg                           *providers.Registry
	pins                          map[string]router.PinSpec
	want, row, exact, callerModel string
	lead                          string // profile the wanted agent leads with; "" = its pin
	current, dispatchID           string
	family                        string // the caller's typed family name; forFamily makes it each candidate's id
	explicitModel, native         bool
	mode                          string     // --fallback: all = requested, pin, claude; agents = no claude
	pf                            *Preflight // findings and notes the candidate checks add
}

// forFamily is rc with a typed family as the exact id cand's own row resolves it to; Claude keeps the tier match.
func (rc routeCtx) forFamily(cand router.Candidate) routeCtx {
	if rc.family == "" || VaultAgentIsClaude(cand.Agent) {
		return rc
	}
	id := providers.ConcreteModel(cand.Agent, router.CandidateRow(rc.reg, cand), rc.family)
	if canonical, _, ok := models.Resolve(id); ok && cand.Profile == "" {
		id = canonical
	}
	rc.exact = id
	if prefix, _, ok := strings.Cut(rc.callerModel, ":"); ok {
		rc.callerModel = prefix + ":" + id
	} else {
		rc.callerModel = id
	}
	return rc
}

// candidateCell adjusts cand's cell for the row or exact model; skip is non-empty when cand serves none.
func (rc routeCtx) candidateCell(cand router.Candidate) (cell router.Cell, skip string) {
	cell = cand.Cell
	switch {
	case VaultAgentIsClaude(cand.Agent) && rc.dispatchID != "": // lowest Claude alias serving the non-Claude dispatch
		rc.tb.ClaudeTierCell(rc.want, rc.dispatchID, &cell)
	case rc.callerOnProfile(cand): // the wanted agent's profile row runs the caller's --model, never its pin or tier
		return rc.callerProfileCell(cand, cell)
	case rc.pinnedCaller(cand):
		rc.pinnedCell(cand, &cell)
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
	return pinned && cand.Agent == rc.want && rc.explicitModel && !rc.callerAuto() &&
		(rc.row == "" || slices.Contains(models.AliasNames, rc.callerModel)) && cmp.Or(rc.exact, rc.callerModel) != ""
}

func (rc routeCtx) pinnedCell(cand router.Candidate, cell *router.Cell) {
	cell.Model = cmp.Or(rc.exact, strings.TrimPrefix(rc.callerModel, cand.Profile+":")) // buildDispatch re-adds the profile
	if rc.reg == nil || rc.reg.Providers[cand.Profile] == nil {
		return
	}
	p := rc.reg.Providers[cand.Profile]
	if slices.Contains(models.AliasNames, rc.callerModel) {
		cell.Model = router.DatabricksModel(cand.Agent, rc.callerModel, p)
		if cell.Model == "" && p.Preset == providers.PresetDatabricks {
			cell.Model = p.Models[rc.callerModel]
		}
		if cell.Model == "" || preflightResponsesMismatch(cand.Agent, p, launchWire(cand.Agent, p, cell.Model)) {
			*cell = cand.Cell // an own-login row or a pin that cannot serve the alias keeps its own cell
			return
		}
	}
	cell.Model = launchWire(cand.Agent, p, cell.Model)
}

// callerOnProfile reports the wanted agent on a registry row asked for a concrete id, or any exact id on Databricks.
func (rc routeCtx) callerOnProfile(cand router.Candidate) bool {
	concrete := rc.explicitModel && cmp.Or(rc.exact, rc.row) != "" && !slices.Contains(models.AliasNames, rc.callerModel)
	p := rc.reg.Row(cand.Profile)
	if !concrete || p == nil || cand.Agent != rc.want {
		return false
	}
	return wireAgent(cand.Agent) || p.Preset == providers.PresetDatabricks && (rc.onWire(cand.Agent) || VaultAgentIsClaude(cand.Agent))
}

// onWire reports agent running the caller's id on a Databricks row: any exact id; a Claude id on codex or the hub.
func (rc routeCtx) onWire(agent string) bool {
	return rc.exact != "" || speaksResponses(agent) || isHubAgent(agent)
}

// callerProfileCell runs the caller's model as cand's profile serves it, or skips the row naming why it cannot.
func (rc routeCtx) callerProfileCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	p := rc.reg.Providers[cand.Profile]
	wire, failed := CheckModelNoted(cand.Agent, p, routedWire(RouteReq{Agent: cand.Agent, Model: rc.callerModel}, p, time.Now()), time.Now(), rc.pf)
	if failed != nil {
		rc.pf.fail(*failed)
		return cell, cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + "): preflight " + failed.String()
	}
	cell.Model = wire
	return cell, ""
}

// profileBacked reports a non-hub profile candidate facing an exact non-Claude model.
func (rc routeCtx) profileBacked(cand router.Candidate) bool {
	return rc.exact != "" && cand.Profile != "" && !isHubAgent(cand.Agent)
}

// olderRow reports an older-row request that cand's own login can serve.
func (rc routeCtx) olderRow(cand router.Candidate) bool {
	return rc.row != "" && rc.row != rc.current && !isHubAgent(cand.Agent) && cand.Profile == ""
}

// exactCell points cell at the cell serving the exact model; skip is non-empty for a non-wanted agent with none.
func (rc routeCtx) exactCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	sc, ok := rc.tb.CellServing(cand.Agent, rc.exact, isHubAgent(cand.Agent))
	if !ok && cand.Agent != rc.want {
		return cell, cand.Agent + ": serves no " + rc.exact
	}
	if ok {
		cell.Model, cell.Effort = sc.Model, sc.Effort
	}
	return cell, ""
}

// profileCell validates a profile candidate against the exact model (codex needs a Responses route), run as spelled.
func (rc routeCtx) profileCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	who := cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + ")"
	var p *providers.Provider
	if rc.reg != nil {
		p = rc.reg.Providers[cand.Profile]
	}
	if speaksResponses(cand.Agent) && p != nil {
		wire := launchWire(cand.Agent, p, rc.exact)
		if !providers.ServesResponses(p, wire) {
			return cell, who + ": serves no " + rc.exact + " over Responses"
		}
		if ready, fresh := router.ServedReady(p, wire, time.Now()); fresh && !ready {
			return cell, who + ": " + rc.exact + " is not a READY endpoint on " + cand.Profile
		}
		cell.Model = wire
		return cell, ""
	}
	if id, _, ok := models.Resolve(cell.Model); (!ok || id != rc.exact) && !router.InFamily(cell.Model, rc.exact) {
		return cell, who + ": serves no " + rc.exact
	}
	return cell, ""
}

func (rc routeCtx) buildDispatch(cand router.Candidate, cell router.Cell) (disp Dispatch, skip string, err error) {
	keepCaller := cand.Agent == rc.want && rc.explicitModel && cand.Profile == "" && !rc.callerAuto()
	if !keepCaller || rc.exact == "" { // a ladder rung's family resolves here, for a step the chain may run
		cell.Model = providers.ConcreteModel(cand.Agent, router.CandidateRow(rc.reg, cand), cell.Model)
	}
	disp = Dispatch{Model: cell.Model, Effort: cell.Effort, Account: cand.Account, Billing: cand.Billing, keepCaller: keepCaller}
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
	return isHubAgent(cand.Agent) && (!rc.native || cand.Agent != rc.want)
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
		disp.Agent, disp.Model = watchFreeDispatch(newHubDispatch("opencode", rc.reg.Providers[cand.Profile], rc.reg), cand.Billing), bare
	case hub:
		r, model, err := hubDispatchFor(rc.reg, cell.Model)
		if err != nil {
			return disp, "opencode: " + err.Error(), nil
		}
		disp.Agent, disp.Model = watchFreeDispatch(r, cand.Billing), model
	default:
		var err error
		disp, err = rc.lookupDispatch(cand, cell, disp)
		return rc.engineModel(cand, cell, disp), "", err
	}
	return disp, "", nil
}

// engineModel names the model gemini really runs on a Databricks row: its engine swaps a non-Gemini cell id at launch.
func (rc routeCtx) engineModel(cand router.Candidate, cell router.Cell, disp Dispatch) Dispatch {
	if p := rc.reg.Row(cand.Profile); agentid.Routing(cand.Agent).GoogleOrDatabricks && GeminiGatewayRow(p) {
		d := mustDriver(cand.Agent).(HeadlessDriver)
		disp.Model = cand.Profile + ":" + d.Before(RunOptions{Model: disp.Model}, providers.Target{Provider: p, Model: cell.Model}).Model
	}
	return disp
}

// lookupDispatch sets disp's engine; Claude keeps a concrete Databricks request, else runs the cell's alias.
func (rc routeCtx) lookupDispatch(cand router.Candidate, cell router.Cell, disp Dispatch) (Dispatch, error) {
	r, err := Lookup(cand.Agent)
	if err != nil {
		return disp, err
	}
	disp.Agent = r
	if VaultAgentIsClaude(cand.Agent) && !rc.callerOnProfile(cand) {
		disp.Model = router.ClaudeCellModel(cell.Model)
		if cand.Profile != "" { // keep the <profile>: spelling so the dispatch resolves on that profile
			disp.Model = cand.Profile + ":" + disp.Model
		}
	}
	return disp, nil
}

func (rc routeCtx) callerAuto() bool { _, ok := SplitAuto(rc.callerModel); return ok }

// servedRepickFor is a codex Databricks dispatch's repick when the switch is on and the caller named no exact model.
func (rc routeCtx) servedRepickFor(cand router.Candidate) func(string) string {
	p := rc.codexDatabricks(cand)
	if p == nil || !providers.ServedRoutingOn() || rc.exact != "" || (rc.callerModel != "" && rc.row == "") {
		return nil
	}
	return func(failed string) string { return router.NextServed(p, failed) }
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
