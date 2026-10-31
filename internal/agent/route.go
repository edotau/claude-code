package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Dispatch is one routed step: the agent plus the cell it runs (model, effort) and the account it bills.
type Dispatch struct {
	Agent                        Runner
	Model, Effort                string
	Account, Billing             string
	note                         string
	repick                       func(failed string) string // byok: skip the failed model, return the next pool model or ""
	servedRepick                 func(failed string) string // codex on Databricks: mark the endpoint gone, return the next
	keepCaller, unroutedDispatch bool                       // keepCaller: the wanted dispatch keeps the caller's own --model
	otherRoute                   bool                       // not the wanted agent's own login or pinned profile
}

// Chain is a routed dispatch: Want is the agent asked for, Dispatches run in order, Skipped are gate drops.
type Chain struct {
	Want       string
	Dispatches []Dispatch
	Skipped    []string
}

// Single is a one-dispatch chain on r with the caller's request untouched (an explicit backend, or a test).
func Single(r Runner) Chain {
	return Chain{Want: r.Name(), Dispatches: []Dispatch{{Agent: r, keepCaller: true, unroutedDispatch: true}}}
}

// State seams: tests point them away from the real quota.json.
var (
	loadState     = router.LoadState
	markExhausted = router.MarkExhausted
	markAuth      = router.MarkAuth
)

// modelRow resolves a --model (or tier) value to the route-table row it names: the tier the pick runs at and the
// canonical Claude id of the row. exact is the canonical id of a non-Claude model, which only agents serving it may run.
func modelRow(model string) (tier, row, exact string) {
	if model == router.BYOKModel {
		return "", "", ""
	}
	canonical, _, ok := models.Resolve(model)
	switch {
	case !ok:
		return "", "", ""
	case models.FamilyKind(canonical) != models.FamilyClaude:
		return "", "", canonical
	}
	tier = cmp.Or(models.AliasOf(canonical), models.FamilyOf(canonical))
	if !slices.Contains(router.Tiers, tier) {
		tier = ""
	}
	return tier, canonical, ""
}

// RouteChain turns a pick at tier (or a Claude row) into dispatches; each dispatch runs that row's cell of its agent.
func RouteChain(ctx context.Context, want, tier, mode string, explicitModel, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	t, row, _ := modelRow(tier)
	return routeChain(ctx, want, cmp.Or(t, tier), row, "", "", mode, explicitModel, native, o)
}

// RouteChainFor routes a caller's --model: an alias or Claude id picks that row's cell; a non-Claude id is exact.
func RouteChainFor(ctx context.Context, want, model, tierHint, mode string, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	tier, row, exact := modelRow(model)
	return routeChain(ctx, want, cmp.Or(tier, tierHint), row, exact, strings.TrimSpace(model), mode, strings.TrimSpace(model) != "", native, o)
}

func routeChain(ctx context.Context, want, tier, row, exact, callerModel, mode string, explicitModel, native bool, o router.PickOpts) (Chain, router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	o.Fallback = mode
	if o.Account == nil {
		o.Account = ResolveAccount
	}
	// --native opencode runs on opencode's own config first: no hub login gate, no table cell
	pickWant, nativeLead := want, native && IsHubAgent(want)
	if nativeLead {
		pickWant = ""
	}
	pick := tb.Pick(ctx, tier, pickWant, loadState(), RosterPick(o))
	reg, _ := providers.Load()
	c := Chain{Want: want}
	if nativeLead {
		c.Dispatches = append(c.Dispatches, Dispatch{Agent: newCLIRunner("opencode"), keepCaller: true, unroutedDispatch: true})
		if mode == router.FallbackNone {
			return c, pick, nil
		}
	}
	for _, s := range pick.Skipped {
		if !nativeLead || !IsHubAgent(s.Agent) {
			c.Skipped = append(c.Skipped, s.Agent+": "+s.Reason)
		}
	}
	current, _ := models.Alias(tier)
	pins := AgentPins()
	rc := routeCtx{tb: tb, reg: reg, pins: pins, want: want, row: row, exact: exact, callerModel: callerModel,
		current: current, dispatchID: nonClaudeDispatchID(exact, explicitModel, pins[want].Model),
		explicitModel: explicitModel, native: native}
	for _, cand := range pick.Candidates {
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
			return Chain{}, pick, err
		}
		if skip != "" {
			c.Skipped = append(c.Skipped, skip)
			continue
		}
		disp = rc.byokCandidate(disp, cand, tier)
		disp.servedRepick = rc.servedRepickFor(cand)
		disp.otherRoute = cand.Agent != want || (want != "claude" && cand.Profile != pins[want].Profile)
		c.Dispatches = append(c.Dispatches, disp)
	}
	return c, pick, nil
}

// routeCtx is the per-pick state routeChain's candidate helpers share.
type routeCtx struct {
	tb                            *router.RouteTable
	reg                           *providers.Registry
	pins                          map[string]router.PinSpec
	want, row, exact, callerModel string
	current, dispatchID           string
	explicitModel, native         bool
}

// candidateCell adjusts cand's cell for the row or exact model; skip is non-empty when cand serves none.
func (rc routeCtx) candidateCell(cand router.Candidate) (cell router.Cell, skip string) {
	cell = cand.Cell
	pinned := cand.Profile != "" && rc.pins[cand.Agent].Profile == cand.Profile
	switch {
	case cand.Agent == "claude" && rc.dispatchID != "": // lowest Claude tier serving the non-Claude dispatch
		rc.claudeTierCell(&cell)
	case pinned && cand.Agent == rc.want && rc.explicitModel && rc.callerModel != router.BYOKModel &&
		rc.row == "" && cmp.Or(rc.exact, rc.callerModel) != "":
		cell.Model = cmp.Or(rc.exact, rc.callerModel) // the caller's model, respelled for the pinned profile
		if rc.reg != nil && rc.reg.Providers[cand.Profile] != nil {
			cell.Model = rc.reg.Providers[cand.Profile].WireModel(cell.Model)
		}
	case rc.exact != "" && cand.Profile != "" && !IsHubAgent(cand.Agent): // a Databricks dispatch serves only its own cell's model
		return rc.profileCell(cand, cell)
	case rc.exact != "": // a non-Claude id runs only where a cell serves it
		sc, ok := cellServing(rc.tb, cand.Agent, rc.exact)
		if !ok && cand.Agent != rc.want {
			return cell, cand.Agent + ": serves no " + rc.exact
		}
		if ok {
			cell.Model, cell.Effort = sc.Model, sc.Effort
		}
	case rc.row != "" && rc.row != rc.current && !IsHubAgent(cand.Agent) && cand.Profile == "": // an older row runs its own cell
		if oc, ok := rc.tb.ByModel[rc.row][cand.Agent]; ok {
			cell.Model, cell.Effort = oc.Model, oc.Effort
		}
	}
	return cell, ""
}

// profileCell validates a profile-backed candidate against the exact model: codex needs a Responses route for it,
// any other agent a cell that already serves it; the accepted cell runs the model as the profile spells it.
func (rc routeCtx) profileCell(cand router.Candidate, cell router.Cell) (router.Cell, string) {
	who := cand.Agent + " (" + cmp.Or(cand.Account, cand.Profile) + ")"
	var p *providers.Provider
	if rc.reg != nil {
		p = rc.reg.Providers[cand.Profile]
	}
	if cand.Agent == "codex" && p != nil {
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

// claudeTierCell points cell at claude's lowest tier serving the non-Claude dispatch model.
func (rc routeCtx) claudeTierCell(cell *router.Cell) {
	t := lowestTierServing(rc.tb, rc.want, rc.dispatchID)
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
	disp.keepCaller = cand.Agent == rc.want && rc.explicitModel && cand.Profile == "" && rc.callerModel != router.BYOKModel
	if disp.keepCaller && rc.exact != "" {
		disp.Model = rc.exact // an exact non-Claude id runs as asked; an alias runs the cell's model
	}
	if cand.Profile != "" { // provider:model pins the dispatch to that Databricks profile, not the session's
		disp.Model = cand.Profile + ":" + cell.Model
	}
	hubOpencode := IsHubAgent(cand.Agent) && (!rc.native || cand.Agent != rc.want)
	if pr := profileRow(rc.reg, cand.Profile); pr != nil && pr.OpencodeID == "" {
		hubOpencode = false // a row with no opencode id can't ride the hub; the native runner takes <profile>:<model>
	}
	switch {
	case hubOpencode && cand.Profile != "" && rc.reg != nil && rc.reg.Providers[cand.Profile] != nil:
		// a pinned registry row runs the hub dispatch on that profile, bare model
		_, bare, ok := strings.Cut(cell.Model, "/")
		if !ok {
			bare = cell.Model
		}
		disp.Agent, disp.Model = watchFreeDispatch(NewHubDispatch("opencode", rc.reg.Providers[cand.Profile], rc.reg), cand.Billing), bare
	case hubOpencode:
		r, model, err := hubDispatchFor(rc.reg, cell.Model)
		if err != nil {
			return disp, "opencode: " + err.Error(), nil
		}
		disp.Agent, disp.Model = watchFreeDispatch(r, cand.Billing), model
	default:
		r, err := Lookup(cand.Agent)
		if err != nil {
			return disp, "", err
		}
		disp.Agent = r
		if cand.Agent == "claude" {
			disp.Model = claudeCellModel(cell.Model)
			if cand.Profile != "" { // keep the <profile>: spelling so the dispatch resolves on that profile
				disp.Model = cand.Profile + ":" + disp.Model
			}
		}
	}
	return disp, "", nil
}

// profileRow is the registry row named name, or nil when absent.
func profileRow(reg *providers.Registry, name string) *providers.Provider {
	if reg == nil || name == "" {
		return nil
	}
	return reg.Providers[name]
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

// lowestTierServing is the lowest router tier (haiku up) whose cell for agentName resolves to id, else "".
// Several tiers may share id (codex gpt-6.1-sol at opus and sonnet); a caller that named an alias never reaches this.
func lowestTierServing(tb *router.RouteTable, agentName, id string) string {
	for _, tier := range slices.Backward(router.Tiers) {
		if c, ok := tb.Rows[agentName][tier]; ok {
			if cid, _, ok := models.Resolve(c.Model); ok && cid == id {
				return tier
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
		for _, tier := range slices.Sorted(maps.Keys(tb.OpencodeSub[sub])) {
			if c := tb.OpencodeSub[sub][tier]; serves(c) {
				return c, true
			}
		}
	}
	return router.Cell{}, false
}

// watchFreeDispatch sets the stall watchdog on r when billing is free; any other agent (or billing) passes through.
func watchFreeDispatch(r Runner, billing string) Runner {
	if billing != router.BillFree {
		return r
	}
	if hl, ok := r.(hubDispatch); ok {
		hl.firstOutput = firstOutputDeadline()
		return hl
	}
	return r
}

// profileAccount is a profile's route-state account key: the structured <provider>·<profile> pair.
func profileAccount(p *providers.Provider) string {
	if p.Provider == "" {
		return p.Name
	}
	return p.Provider + "·" + p.Name
}

// ResolveAccount is resolveAccount for callers outside the package (the gateway router view).
var ResolveAccount = resolveAccount

// resolveAccount is the account an agent's dispatch actually bills: "" keeps the table's key (the agent's own login
// or subscription); an agent resolving to another profile (an API key, a BYOK route) bills that <provider>·<profile>.
func resolveAccount(agentName string, c router.Cell) (string, string) {
	var t providers.Target
	switch agentName {
	case "opencode":
		return "", "" // the cell's provider prefix is the account; Configured gates its credential
	case "claude":
		_, ct, err := resolveTarget(RunOptions{Provider: providers.SessionProfile(), Model: c.Model})
		if err != nil || ct.Provider == nil || ct.Provider.Auth.Method() == providers.AuthCLI {
			return "", ""
		}
		t = ct
	default:
		if ownLoginAccount(agentName, c.Account) {
			return "", "" // the agent's own login cell keeps its account; a session profile never replaces it
		}
		vt, own, _, err := schemaFor(agentName).targetNote(RunOptions{Provider: sessionProvider(agentName), Model: c.Model})
		if err != nil || own || vt.Provider == nil || vt.Provider.Auth.Login == agentName {
			return "", ""
		}
		t = vt
	}
	return profileAccount(t.Provider), billingOf(t.Provider)
}

// sessionProvider is the session's profile when it serves the agent, else "" (the vault pin and fallbacks decide).
func sessionProvider(agentName string) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	if p := reg.Providers[providers.SessionProfile()]; p != nil && (agentName == "claude" || CanServe(agentName, p)) {
		return p.Name
	}
	return ""
}

// ownLoginAccount is true when account is the <provider>·<profile> of a profile the agent's own CLI login backs.
func ownLoginAccount(agentName, account string) bool {
	_, profile, ok := strings.Cut(account, "·")
	if !ok {
		return false
	}
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p := reg.Providers[profile]
	return p != nil && p.Auth.Login == agentName && profileAccount(p) == account
}

// billingOf: a harness-held key bills per use, anything else is a plan.
func billingOf(p *providers.Provider) string {
	if p.APIKeyAuth() {
		return router.BillCredits
	}
	return router.BillSubscription
}

// ExplicitChain is r alone on the caller's explicit backend (--provider or provider:model), carrying that backend's
// account: a remembered exhaustion skips it without an attempt, and a quota failure is recorded on it.
func ExplicitChain(r Runner, req RunOptions) Chain {
	c := Single(r)
	acct, billing := explicitAccount(r, req)
	c.Dispatches[0].Account, c.Dispatches[0].Billing = acct, billing
	if req.Model == router.BYOKModel {
		reg, _ := providers.Load()
		c.Dispatches[0] = byokDispatch(c.Dispatches[0], router.Tiers[1], reg, req.Provider, false)
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

// WithTier runs c's lone explicit dispatch on the router's tier slot when the caller named no model.
func WithTier(c Chain, tier string) Chain {
	if len(c.Dispatches) != 1 || tier == "" {
		return c
	}
	disp := c.Dispatches[0]
	disp.Model, disp.keepCaller, disp.unroutedDispatch = tier, false, false
	c.Dispatches = []Dispatch{disp}
	return c
}

// explicitAccount is the <provider>·<profile> key of the named backend; a hub dispatch names its own profile.
func explicitAccount(r Runner, req RunOptions) (string, string) {
	if l, ok := r.(hubDispatch); ok {
		if OpencodeCredID(l.provider.OpencodeID) == "opencode" {
			return "opencode·go", router.BillSubscription
		}
		return profileAccount(l.provider), billingOf(l.provider)
	}
	reg, err := providers.Load()
	if err != nil {
		return "", ""
	}
	name := req.Provider
	if name == "" {
		name, _, _ = strings.Cut(req.Model, ":")
	}
	p, err := reg.Get(name)
	if err != nil {
		return "", ""
	}
	if p.Auth.Login == "copilot" { // the GitHub login bills the pinned profile
		return copilotLoginAccount(), billingOf(p)
	}
	return profileAccount(p), billingOf(p)
}

// copilotLoginAccount is the pinned copilot profile's route-state key (github·<profile>), or "" when no user is pinned.
func copilotLoginAccount() string {
	if u := router.CopilotUser(); u != "" {
		return "github·" + u
	}
	return ""
}

// hubDispatchFor maps an opencode cell "<opencodeID>/<model>" to the hub dispatch on that provider and the bare model.
func hubDispatchFor(reg *providers.Registry, cell string) (Runner, string, error) {
	id, model, ok := strings.Cut(cell, "/")
	if !ok || reg == nil {
		return nil, "", fmt.Errorf("cell %q is not <opencodeID>/<model>", cell)
	}
	var first *providers.Provider
	for _, name := range reg.Names() {
		p := reg.Providers[name]
		if p.OpencodeID != id {
			continue
		}
		if providers.CredentialPresent(p) { // rows can share an opencodeID (chatgpt, openai): the signed-in one serves it
			return hubDispatch{agent: "opencode", provider: p, reg: reg}, model, nil
		}
		if first == nil {
			first = p
		}
	}
	if first != nil {
		return hubDispatch{agent: "opencode", provider: first, reg: reg}, model, nil
	}
	// opencode's own plans (opencode-go, opencode) have no registry provider: opencode resolves them itself.
	if OpencodeCredID(id) == "opencode" {
		return hubDispatch{agent: "opencode", provider: &providers.Provider{Name: id, OpencodeID: id}, reg: reg}, model, nil
	}
	return nil, "", fmt.Errorf("no provider has opencodeID %q", id)
}

// dispatchRequest is req for dispatch: the wanted dispatch keeps an explicit caller model; every other dispatch drops the original
// provider and session (they belong to the dispatch that minted them) and runs its cell, the caller's --effort winning.
func dispatchRequest(req RunOptions, disp Dispatch, first bool) RunOptions {
	disp.byokNote()
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

// exhaustedSince is a KindQuota error when dispatch's account is marked exhausted right now, else nil.
func exhaustedSince(disp Dispatch) *Error {
	if disp.Account == "" || disp.Account == copilotLoginAccount() { // copilot's own gate falls back to Databricks instead
		return nil
	}
	now := time.Now()
	e := loadState()[disp.Account]
	if e.Status(now) != router.StateExhausted {
		return nil
	}
	return &Error{Kind: KindQuota, Agent: disp.Agent.Name(), Err: errors.New(router.ExhaustedReason(disp.Account, e, now))}
}

// authMarkedSince is a KindAuth error when dispatch's account was marked auth-dead after the chain was built, else nil.
func authMarkedSince(disp Dispatch) *Error {
	if disp.Account == "" {
		return nil
	}
	reason, marked := loadState().AuthMarked(disp.Account, time.Now())
	if !marked {
		return nil
	}
	return &Error{Kind: KindAuth, Agent: disp.Agent.Name(), AuthReason: reason, Err: errors.New(disp.Account + ": " + reason)}
}

// noteQuota remembers a KindQuota failure on the dispatch's account until its parsed reset (else the known window).
func noteQuota(disp Dispatch, err error) {
	var e *Error
	if disp.Account == "" || !errors.As(err, &e) || e.Kind != KindQuota {
		return
	}
	now := time.Now()
	until := router.ResetFor(err.Error(), disp.Billing, loadState()[disp.Account], now)
	_ = markExhausted(disp.Account, until, now, fallbackReason(err)) // best-effort: the run's own error still reports it
}

// noteStall remembers a stalled free dispatch for 15 minutes, so the sub-pick skips it instead of hanging again.
func noteStall(disp Dispatch, err error) {
	if disp.Account == "" || !errors.Is(err, errStalled) {
		return
	}
	now := time.Now()
	reason := fmt.Sprintf("free dispatch stalled (no output in %s)", firstOutputDeadline())
	_ = markExhausted(disp.Account, now.Add(15*time.Minute), now, reason) // best-effort: the run's own error still reports it
}

// condemnAuth tags a KindAuth whose Recover failed with a condemning verdict, or any error that itself condemns
// (an initial u2m invalid_grant / missing refresh token can classify as fatal with no Recover run).
func condemnAuth(e *Error, recoverErr error) {
	switch {
	case e.Kind == KindAuth && providers.Verdict(recoverErr).Condemns():
		e.AuthReason = fallbackReason(recoverErr)
	case e.Err != nil && providers.Verdict(e.Err).Condemns():
		e.AuthReason = fallbackReason(e.Err)
	}
}

// noteAuth marks the dispatch's account auth-dead for a condemned KindAuth, so Pick skips it until a login clears it.
func noteAuth(disp Dispatch, err error) {
	var e *Error
	if disp.Account == "" || !errors.As(err, &e) || e.AuthReason == "" {
		return
	}
	_ = markAuth(disp.Account, e.AuthReason) // best-effort: the run's own error still reports it
}

// RosterPick fills o's unset Configured, Pins, CanServe and CodexModel from the roster (the router imports no agent).
func RosterPick(o router.PickOpts) router.PickOpts {
	if o.Configured == nil {
		o.Configured = Configured
	}
	if o.Pins == nil {
		o.Pins = AgentPins()
	}
	if o.CanServe == nil {
		o.CanServe = CanServe
	}
	if o.CodexModel == nil {
		o.CodexModel = func(t providers.Target) providers.Target {
			t, _ = ResolveAgentModel(ModelAsk{Agent: "codex", Target: t})
			return t
		}
	}
	return o
}
