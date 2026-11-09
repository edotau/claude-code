package agent

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

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
	pins, err := AgentPins()
	return o, pins, err
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

// Resolve is the one route entry: lead (flags, session, arm), explicit backend, routed ladder, or a no-run preview.
func Resolve(ctx context.Context, q RouteReq) (Chain, router.PickResult, error) {
	if q.Host {
		return Single(q.Engine), router.PickResult{}, nil
	}
	lead, explicit, err := routeLead(q)
	if err != nil {
		return Chain{}, router.PickResult{}, err
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
	return RouteChainFor(ctx, q.Agent, q.Model, q.AliasHint, q.Fallback, q.Native,
		router.PickOpts{Lead: lead, Refresh: q.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(q.WorkDir) }})
}

// routeLead is LeadFor plus the arm: an unled routed run (and preview) takes ArmLead.
func routeLead(q RouteReq) (lead string, explicit bool, err error) {
	lead, explicit, err = LeadFor(q)
	if err != nil {
		return "", false, err
	}
	if explicit && q.Preview { // a preview routes a pin (or a profile:model) as its lead, never the arm
		return q.Profile, false, nil
	}
	if explicit || lead != "" || q.Native {
		return lead, explicit, nil
	}
	if armValue(q.Agent) == "" { // arm off or no wanted agent: skip the registry load
		return "", false, nil
	}
	lead, err = ArmLead(armRegistry(), q.Agent)
	return lead, false, err
}

// armRegistry loads the registry for routeLead's arm; a package var so a test can count loads.
var armRegistry = loadRegistryWarn

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
	lead, err = ResolveLead(reg, q.Agent, q.Profile, q.Provider)
	return lead, false, err
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

// EnvDatabricksArm (vault env) defaults every non-claude lead to Databricks: off|session|<databricks profile>.
const EnvDatabricksArm = "HARNESS_DATABRICKS_ARM"

// armValue is EnvDatabricksArm's setting for agentName; "" when the arm is off or no agent besides claude is wanted.
func armValue(agentName string) string {
	v := strings.TrimSpace(os.Getenv(EnvDatabricksArm))
	if v == "off" || agentName == "claude" || agentName == "" {
		return ""
	}
	return v
}

// ArmLead is the lead EnvDatabricksArm gives a dispatch that names no --profile or --provider; "" when off.
func ArmLead(reg *providers.Registry, agentName string) (string, error) {
	v := armValue(agentName)
	if v == "" {
		return "", nil
	}
	if reg == nil {
		return "", fmt.Errorf("%s=%s: no provider registry", EnvDatabricksArm, v)
	}
	lead, err := ResolveLead(reg, agentName, v, "databricks")
	if err != nil {
		return "", fmt.Errorf("%s=%s: %w", EnvDatabricksArm, v, err)
	}
	return lead, nil
}

// LeadSession is the --profile value naming the session's own profile (CLAUDE_PROFILE, else claude's record).
const LeadSession = "session"

// ResolveLead is the profile a routed run leads with: profile (LeadSession = the session's), narrowed to provider.
func ResolveLead(reg *providers.Registry, agentName, profile, provider string) (string, error) {
	fromSession := profile == LeadSession
	if fromSession {
		profile = providers.SessionLead(nil, true)
		// a session-inherited lead never moves gemini off its pin; only a named profile or provider does
		if p := reg.Providers[profile]; agentName == "gemini" && provider == "" && (p == nil || p.Provider != "google") {
			return "", nil
		}
	}
	if profile != "" && reg.Providers[profile] == nil {
		return "", fmt.Errorf("profile %s is not in the registry", profile)
	}
	if provider == "" {
		return profile, nil
	}
	if p := reg.Providers[profile]; p != nil && p.Provider == provider {
		return profile, nil
	}
	if profile != "" && !fromSession {
		return "", fmt.Errorf("--profile %s is not a %s profile", profile, provider)
	}
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; CanServe(agentName, p) && providers.CredentialPresent(p) {
			return n, nil
		}
	}
	return "", fmt.Errorf("no signed-in %s profile serves %s", provider, agentName)
}
