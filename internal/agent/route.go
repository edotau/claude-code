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
	"github.com/edotau/claude-code/internal/routetable"
	"github.com/edotau/claude-code/internal/vendors"
)

// Leg is one routed step: the agent plus the cell it runs (model, effort) and the account it bills.
type Leg struct {
	Agent                   Runner
	Model, Effort           string
	Account, Billing        string
	keepCaller, unroutedLeg bool // keepCaller: the wanted leg keeps the caller's own --model
	otherRoute              bool // otherRoute: not the wanted agent's requested route (own login, or its pinned profile)
}

// Chain is a routed dispatch: Want is the agent asked for, Legs run in order, Skipped are gate drops.
type Chain struct {
	Want    string
	Legs    []Leg
	Skipped []string
}

// Single is a one-leg chain on r with the caller's request untouched (an explicit backend, or a test).
func Single(r Runner) Chain {
	return Chain{Want: r.Name(), Legs: []Leg{{Agent: r, keepCaller: true, unroutedLeg: true}}}
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

// RouteChain turns a pick at tier (or a Claude row) into legs; each leg runs that row's cell of its agent.
func RouteChain(ctx context.Context, want, tier, mode string, explicitModel, native bool, o router.Opts) (Chain, router.Result, error) {
	t, row, _ := modelRow(tier)
	return routeChain(ctx, want, cmp.Or(t, tier), row, "", "", mode, explicitModel, native, o)
}

// RouteChainFor routes a caller's --model: an alias or Claude id picks that row's cell; a non-Claude id is exact.
func RouteChainFor(ctx context.Context, want, model, tierHint, mode string, native bool, o router.Opts) (Chain, router.Result, error) {
	tier, row, exact := modelRow(model)
	return routeChain(ctx, want, cmp.Or(tier, tierHint), row, exact, strings.TrimSpace(model), mode, strings.TrimSpace(model) != "", native, o)
}

func routeChain(ctx context.Context, want, tier, row, exact, callerModel, mode string, explicitModel, native bool, o router.Opts) (Chain, router.Result, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.Result{}, err
	}
	o.Fallback = mode
	if o.Account == nil {
		o.Account = ResolveAccount
	}
	// --native opencode runs on opencode's own config first: no hub login gate, no table cell
	pickWant, nativeLead := want, native && want == "opencode"
	if nativeLead {
		pickWant = ""
	}
	pick := tb.Pick(ctx, tier, pickWant, loadState(), o)
	reg, _ := providers.Load()
	c := Chain{Want: want}
	if nativeLead {
		c.Legs = append(c.Legs, Leg{Agent: vendorRunner{"opencode"}, keepCaller: true, unroutedLeg: true})
		if mode == FallbackNone {
			return c, pick, nil
		}
	}
	for _, s := range pick.Skipped {
		if !nativeLead || s.Agent != "opencode" {
			c.Skipped = append(c.Skipped, s.Agent+": "+s.Reason)
		}
	}
	current, _ := models.Alias(tier)
	pins := providers.AgentPins()
	rc := routeCtx{tb: tb, reg: reg, pins: pins, want: want, row: row, exact: exact, callerModel: callerModel,
		current: current, legID: nonClaudeLegID(exact, explicitModel, pins[want].Model),
		explicitModel: explicitModel, native: native}
	for _, cand := range pick.Candidates {
		if nativeLead && cand.Agent == "opencode" {
			continue
		}
		cell, skip := rc.candidateCell(cand)
		if skip != "" {
			c.Skipped = append(c.Skipped, skip)
			continue
		}
		leg, skip, err := rc.buildLeg(cand, cell)
		if err != nil {
			return Chain{}, pick, err
		}
		if skip != "" {
			c.Skipped = append(c.Skipped, skip)
			continue
		}
		leg.otherRoute = cand.Agent != want || (want != "claude" && cand.Profile != pins[want].Profile)
		c.Legs = append(c.Legs, leg)
	}
	return c, pick, nil
}

// routeCtx is the per-pick state routeChain's candidate helpers share.
type routeCtx struct {
	tb                            *router.Table
	reg                           *providers.Registry
	pins                          map[string]providers.AgentPinSpec
	want, row, exact, callerModel string
	current, legID                string
	explicitModel, native         bool
}

// candidateCell is the cell cand runs, adjusted for the pick's row or exact model; skip is non-empty when cand serves none.
func (rc routeCtx) candidateCell(cand router.Candidate) (cell router.Cell, skip string) {
	cell = cand.Cell
	pinned := cand.Profile != "" && rc.pins[cand.Agent].Profile == cand.Profile
	switch {
	case cand.Agent == "claude" && rc.legID != "": // Claude's tier for a non-Claude leg: the lowest tier whose cell serves it
		rc.claudeTierCell(&cell)
	case pinned && cand.Agent == rc.want && rc.explicitModel && rc.row == "" && cmp.Or(rc.exact, rc.callerModel) != "":
		cell.Model = cmp.Or(rc.exact, rc.callerModel) // the caller's model, respelled for the pinned profile
		if rc.reg != nil && rc.reg.Providers[cand.Profile] != nil {
			cell.Model = rc.reg.Providers[cand.Profile].WireModel(cell.Model)
		}
	case rc.exact != "" && cand.Profile != "" && cand.Agent != "opencode": // a Databricks leg serves only its own cell's model
		return rc.profileCell(cand, cell)
	case rc.exact != "": // a non-Claude id runs only where a cell serves it
		sc, ok := cellServing(rc.tb, cand.Agent, rc.exact)
		if !ok && cand.Agent != rc.want {
			return cell, cand.Agent + ": serves no " + rc.exact
		}
		if ok {
			cell.Model, cell.Effort = sc.Model, sc.Effort
		}
	case rc.row != "" && rc.row != rc.current && cand.Agent != "opencode" && cand.Profile == "": // an older row runs its own cell
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
		cell.Model = wire
		return cell, ""
	}
	if id, _, ok := models.Resolve(cell.Model); !ok || id != rc.exact {
		return cell, who + ": serves no " + rc.exact
	}
	return cell, ""
}

// claudeTierCell points cell at claude's lowest tier serving the non-Claude leg model.
func (rc routeCtx) claudeTierCell(cell *router.Cell) {
	t := lowestTierServing(rc.tb, rc.want, rc.legID)
	if t == "" {
		return
	}
	if tc, ok := rc.tb.Rows["claude"][t]; ok {
		cell.Model, cell.Effort = tc.Model, tc.Effort
	}
}

// buildLeg turns cand's cell into a leg; skip is non-empty when the leg cannot run, err when its agent is unknown.
func (rc routeCtx) buildLeg(cand router.Candidate, cell router.Cell) (leg Leg, skip string, err error) {
	leg = Leg{Model: cell.Model, Effort: cell.Effort, Account: cand.Account, Billing: cand.Billing}
	leg.keepCaller = cand.Agent == rc.want && rc.explicitModel && cand.Profile == ""
	if leg.keepCaller && rc.exact != "" {
		leg.Model = rc.exact // an exact non-Claude id runs as asked; an alias runs the cell's model
	}
	if cand.Profile != "" { // provider:model pins the leg to that Databricks profile, not the session's
		leg.Model = cand.Profile + ":" + cell.Model
	}
	hubOpencode := cand.Agent == "opencode" && (!rc.native || cand.Agent != rc.want)
	switch {
	case hubOpencode && cand.Profile != "" && rc.reg != nil && rc.reg.Providers[cand.Profile] != nil:
		// a pinned registry row runs the hub leg on that profile, bare model
		_, bare, ok := strings.Cut(cell.Model, "/")
		if !ok {
			bare = cell.Model
		}
		leg.Agent, leg.Model = watchFreeLeg(NewHubLeg("opencode", rc.reg.Providers[cand.Profile], rc.reg), cand.Billing), bare
	case hubOpencode:
		r, model, err := hubLegFor(rc.reg, cell.Model)
		if err != nil {
			return leg, "opencode: " + err.Error(), nil
		}
		leg.Agent, leg.Model = watchFreeLeg(r, cand.Billing), model
	default:
		r, err := Lookup(cand.Agent)
		if err != nil {
			return leg, "", err
		}
		leg.Agent = r
		if cand.Agent == "claude" {
			leg.Model = claudeCellModel(cell.Model)
			if cand.Profile != "" { // keep the <profile>: spelling so the leg resolves on that profile
				leg.Model = cand.Profile + ":" + leg.Model
			}
		}
	}
	return leg, "", nil
}

// nonClaudeLegID is the canonical non-Claude model the leg runs: the exact --model, else the wanted agent's model pin.
func nonClaudeLegID(exact string, explicitModel bool, pinModel string) string {
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
func lowestTierServing(tb *router.Table, agentName, id string) string {
	for _, tier := range slices.Backward(router.Tiers) {
		if c, ok := tb.Rows[agentName][tier]; ok {
			if cid, _, ok := models.Resolve(c.Model); ok && cid == id {
				return tier
			}
		}
	}
	return ""
}

// claudeCellModel is the model a routed claude leg runs: the alias a canonical row id backs, so the session's
// selection (CLAUDE_<TIER>_MODEL pins, auto-[1m]) resolves it; an older row without an alias stays its canonical id.
func claudeCellModel(cell string) string {
	canonical, _, ok := models.Resolve(cell)
	if !ok {
		return cell
	}
	return cmp.Or(models.AliasOf(canonical), canonical)
}

// cellServing is agentName's table cell (any row, then opencode's sub-candidates) whose model is exact.
func cellServing(tb *router.Table, agentName, exact string) (router.Cell, bool) {
	serves := func(c router.Cell) bool { id, _, ok := models.Resolve(c.Model); return ok && id == exact }
	for _, row := range slices.Sorted(maps.Keys(tb.ByModel)) {
		if c, ok := tb.ByModel[row][agentName]; ok && serves(c) {
			return c, true
		}
	}
	if agentName != "opencode" {
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

// watchFreeLeg sets the stall watchdog on r when billing is free; any other agent (or billing) passes through.
func watchFreeLeg(r Runner, billing string) Runner {
	if billing != router.BillFree {
		return r
	}
	if hl, ok := r.(hubLeg); ok {
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
		return "", "" // the cell's provider prefix is the account; router.Configured gates its credential
	case "claude":
		_, ct, err := resolveTarget(Request{Provider: providers.SessionProfile(), Model: c.Model})
		if err != nil || ct.Provider == nil || ct.Provider.Auth.Method() == providers.AuthCLI {
			return "", ""
		}
		t = ct
	default:
		if ownLoginAccount(agentName, c.Account) {
			return "", "" // the agent's own login cell keeps its account; a session profile never replaces it
		}
		vt, own, _, err := vendorTargetNote(Request{Provider: sessionProvider(agentName), Model: c.Model}, agentName)
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
	if p := reg.Providers[providers.SessionProfile()]; p != nil && (agentName == "claude" || vendors.CanServe(agentName, p)) {
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
func ExplicitChain(r Runner, req Request) Chain {
	c := Single(r)
	acct, billing := explicitAccount(r, req)
	c.Legs[0].Account, c.Legs[0].Billing = acct, billing
	if acct == "" || acct == copilotLoginAccount() { // copilot's own gate falls back to Databricks instead
		return c
	}
	now := time.Now()
	if e := loadState()[acct]; e.Status(now) == router.StateExhausted {
		return Chain{Want: r.Name(), Skipped: []string{r.Name() + ": " + router.ExhaustedReason(acct, e, now)}}
	}
	return c
}

// explicitAccount is the <provider>·<profile> key of the named backend; a hub leg names its own profile.
func explicitAccount(r Runner, req Request) (string, string) {
	if l, ok := r.(hubLeg); ok {
		if vendors.OpencodeCredID(l.provider.OpencodeID) == "opencode" {
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
	if u := routetable.CopilotUser(); u != "" {
		return "github·" + u
	}
	return ""
}

// hubLegFor maps an opencode cell "<opencodeID>/<model>" to the hub leg on that provider and the bare model.
func hubLegFor(reg *providers.Registry, cell string) (Runner, string, error) {
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
			return hubLeg{agent: "opencode", provider: p, reg: reg}, model, nil
		}
		if first == nil {
			first = p
		}
	}
	if first != nil {
		return hubLeg{agent: "opencode", provider: first, reg: reg}, model, nil
	}
	// opencode's own plans (opencode-go, opencode) have no registry provider: opencode resolves them itself.
	if vendors.OpencodeCredID(id) == "opencode" {
		return hubLeg{agent: "opencode", provider: &providers.Provider{Name: id, OpencodeID: id}, reg: reg}, model, nil
	}
	return nil, "", fmt.Errorf("no provider has opencodeID %q", id)
}

// legRequest is req for leg: the wanted leg keeps an explicit caller model; every other leg drops the original
// provider and session (they belong to the leg that minted them) and runs its cell, the caller's --effort winning.
func legRequest(req Request, leg Leg, first bool) Request {
	out := req
	if leg.unroutedLeg {
		return out
	}
	if !first {
		out.Provider, out.SessionID = "", ""
	}
	if !leg.keepCaller {
		out.Model = leg.Model
	}
	if out.Effort == "" {
		out.Effort = leg.Effort
	}
	return out
}

// noteQuota remembers a KindQuota failure on the leg's account until its parsed reset (else the known window).
func noteQuota(leg Leg, err error) {
	var e *Error
	if leg.Account == "" || !errors.As(err, &e) || e.Kind != KindQuota {
		return
	}
	now := time.Now()
	until := router.ResetFor(err.Error(), leg.Billing, loadState()[leg.Account], now)
	_ = markExhausted(leg.Account, until, now, fallbackReason(err)) // best-effort: the run's own error still reports it
}

// noteStall remembers a stalled free leg for 15 minutes, so the sub-pick skips it instead of hanging again.
func noteStall(leg Leg, err error) {
	if leg.Account == "" || !errors.Is(err, errStalled) {
		return
	}
	now := time.Now()
	reason := fmt.Sprintf("free leg stalled (no output in %s)", firstOutputDeadline())
	_ = markExhausted(leg.Account, now.Add(15*time.Minute), now, reason) // best-effort: the run's own error still reports it
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

// noteAuth marks the leg's account auth-dead for a condemned KindAuth, so router.Pick skips it until a login clears it.
func noteAuth(leg Leg, err error) {
	var e *Error
	if leg.Account == "" || !errors.As(err, &e) || e.AuthReason == "" {
		return
	}
	_ = markAuth(leg.Account, e.AuthReason) // best-effort: the run's own error still reports it
}
