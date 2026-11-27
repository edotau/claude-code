package agent

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/rtkwrap"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/router"
)

// withOwnHopCodemodeNote notes each own-CLI hop that runs code mode off (hopCodemode applies it per hop).
func withOwnHopCodemodeNote(q RouteReq, c Chain) Chain {
	if !rtkwrap.CodemodeOn(q.Opts.Codemode) {
		return c
	}
	for _, d := range c.Dispatches {
		if ownCLICodemodeOff(d.Agent.Name()) {
			c = c.noted("codemode: off for " + d.Agent.Name() + " (" + ownCLIReason + ")")
		}
	}
	return c
}

// ownDatabricksHop reports lead as the asked agent's own hop on a Databricks row.
func ownDatabricksHop(reg *providers.Registry, q RouteReq, lead Dispatch) bool {
	return reg != nil && lead.Agent.Name() == q.Agent && databricksDispatch(reg, lead)
}

// carriedChild reports this ask as the inner line of a --carry of agent: its parent dispatch record carries agent.
func carriedChild(ctx context.Context, agent string) bool {
	id := proc.Value(ctx, proc.DispatchIDEnv)
	pr, ok := parentRecord(id)
	return ok && pr.ID == id && pr.Request.Carry && pr.Agent == agent && pr.Status == "running" &&
		liveAncestor(pr.PID, pr.PIDStart, id)
}

// piRunnerHops is the pi hops router.Chain orders from lead's profile and model, each validated; why when none run.
func piRunnerHops(ctx context.Context, reg *providers.Registry, q RouteReq, lead Dispatch, pi Engine) ([]Dispatch, []string, string) {
	_, profile, _ := strings.Cut(lead.Account, "·")
	if arm := router.ArmProfileContext(ctx); arm != "" && arm != profile {
		return nil, nil, hopTarget(lead) + " is not on the confined workspace " + arm
	}
	peeked, memo := withPeekMemo(ctx)
	model, wire, why := piRunnerModel(peeked, reg, q, lead, profile) // the head is validated once: a rejected head runs none
	if why != "" {
		return nil, nil, why
	}
	sibs, skips, why := piSiblings(ctx, reg, lead, model)
	if why != "" {
		return nil, nil, why
	}
	if why := stampedPick(ctx, reg, q, lead, *memo); why != "" { // stamped last: a refused hop stamps nothing
		return nil, nil, why
	}
	hops := append([]Dispatch{headPiHop(reg, lead, profile, wire)}, sibs...)
	for i := range hops {
		hops[i].Agent, hops[i].runFor = pi, q.Agent
	}
	return hops, skips, ""
}

// piSiblings is the validated pi hops on the plan's pool workspaces after lead's own.
func piSiblings(ctx context.Context, reg *providers.Registry, lead Dispatch, model string) ([]Dispatch, []string, string) {
	plan, _ := PlanOf(ctx)
	if staysOnWorkspace(plan, lead) {
		return nil, nil, ""
	}
	_, profile, _ := strings.Cut(lead.Account, "·")
	var hops []Dispatch
	var skips []string
	for _, w := range plan.Pool[min(1, len(plan.Pool)):] {
		if w.Profile == profile {
			continue
		}
		d, why := piHop(reg, router.ChainHop{Agent: agentid.Pi, Profile: w.Profile, Account: w.Account, Model: model}, lead.Effort)
		if why != "" {
			skips = append(skips, why)
			continue
		}
		hops = append(hops, d)
	}
	return hops, skips, ""
}

// staysOnWorkspace: an explicit backend or a plan that keeps every hop on its first workspace.
func staysOnWorkspace(plan RoutePlan, lead Dispatch) bool {
	return lead.unroutedDispatch || plan.Fallback.Kind == router.PolicySameWorkspace
}

// headPiHop is the pi runner dispatch on lead's own workspace: lead's route flags on the wire piRunnerModel validated.
func headPiHop(reg *providers.Registry, lead Dispatch, profile, wire string) Dispatch {
	return Dispatch{Model: wire, Effort: lead.Effort, Route: rowRoute(reg, profile), Account: lead.Account,
		Billing: lead.Billing, otherRoute: lead.otherRoute, swap: lead.swap, routerChose: lead.routerChose, rung: lead.rung}
}

// piHop is the pi runner dispatch for sibling hop h once it serves the model on Pi's menu and passes its preflight.
func piHop(reg *providers.Registry, h router.ChainHop, effort string) (Dispatch, string) {
	p := reg.Row(h.Profile)
	if p == nil {
		return Dispatch{}, "pi (" + h.Profile + "): not a registry row"
	}
	wire, why := ProfileServes(agentid.Pi, p, h.Model)
	if why == "" {
		why = piReady(p, wire)
	}
	if why != "" {
		return Dispatch{}, "pi (" + h.Profile + "): " + why
	}
	return Dispatch{Model: wire, Effort: effort, Route: rowRoute(reg, h.Profile), Account: h.Account,
		Billing: router.DefaultBilling(p), otherRoute: true}, ""
}

// piReady is why a sibling cannot run wire on Pi (local checks): off its menu, or no credential the proxy can use.
func piReady(p *providers.Provider, wire string) string {
	if !piMenuHas(p, wire) {
		return wire + " is not on pi's " + p.Name + " menu"
	}
	if !providers.CredentialPresent(p) {
		return "no stored credential for " + p.Name
	}
	if f, dead := deadLoginFinding(p); dead {
		return f.Detail
	}
	return ""
}

// piMenuHas reports wire on p's Pi menu (the driver's Runnable, the set its config writes); no such driver passes.
func piMenuHas(p *providers.Provider, wire string) bool {
	d, ok := driverFor(agentid.Pi)
	pr, can := d.(PoolRunnable)
	return !ok || !can || pr.Runnable(p, wire)
}

// piRunnerModel is the model q.Agent resolves on profile (the resolver gateway resolve uses) and its wire on Pi's menu.
func piRunnerModel(ctx context.Context, reg *providers.Registry, q RouteReq, lead Dispatch, profile string) (model, wire, why string) {
	want, why := runnerWant(q, lead)
	if why != "" {
		return "", "", why
	}
	model, why = agentModelOn(PeekContext(ctx), reg, q, want, profile) // the typed model first (R8)
	if why == "" {
		why = ownHopDiffers(ctx, reg, q, lead, model)
	}
	if why != "" {
		return "", "", why
	}
	wire, why = piWire(reg, profile, model)
	if why != "" {
		return "", "", "pi on " + profile + ": " + why
	}
	return model, wire, ""
}

// piWire is model's wire id on profile's Pi menu, or why Pi cannot run it there.
func piWire(reg *providers.Registry, profile, model string) (wire, why string) {
	p := reg.Row(profile)
	if wire, why = ProfileServes(agentid.Pi, p, model); why == "" && !piMenuHas(p, wire) {
		why = wire + " is not on pi's menu"
	}
	return wire, why
}

// stampedPick stamps the rotating pick only if it is still the peeked one; why when it moved or none was validated.
func stampedPick(ctx context.Context, reg *providers.Registry, q RouteReq, lead Dispatch, peeked string) string {
	if !IsAuto(lead.Model) || q.Preview || Peeking(ctx) {
		return ""
	}
	_, profile, _ := strings.Cut(lead.Account, "·")
	if peeked == "" {
		return "pi on " + profile + ": no rotation pick was validated"
	}
	if _, why := agentModelOn(router.WithExpectedPick(ctx, peeked), reg, q, AutoModel, profile); why != "" {
		return "pi on " + profile + ": " + why
	}
	return ""
}

// runnerWant is the model word the pi hop resolves: auto only for a rotating agent, whose own pick Pi then runs.
func runnerWant(q RouteReq, lead Dispatch) (want, why string) {
	switch {
	case q.AutoModel || IsAuto(q.Model):
		return "", autoPickWhy
	case !IsAuto(lead.Model):
		return cmp.Or(q.Model, lead.Model), ""
	case q.Model != "" || !rotatesUnderAuto(q.Agent):
		return "", autoPickWhy
	}
	return AutoModel, ""
}

// ownHopDiffers is why lead's own CLI hop would run another model than model, "" when both hops run one model.
func ownHopDiffers(ctx context.Context, reg *providers.Registry, q RouteReq, lead Dispatch, model string) string {
	if q.Model == "" || lead.Model == "" || lead.Model == q.Model {
		return ""
	}
	_, profile, _ := strings.Cut(lead.Account, "·")
	if own, why := agentModelOn(PeekContext(ctx), reg, q, lead.Model, profile); why != "" || !models.SameModel(own, model) {
		return "the agent's own hop runs " + lead.Model + ", not " + model
	}
	return ""
}

const autoPickWhy = "an auto pick runs on the agent's own CLI"

// agentModelOn is model as q.Agent resolves it on profile, or why it is no fixed model there (an error, an auto pick).
func agentModelOn(ctx context.Context, reg *providers.Registry, q RouteReq, model, profile string) (string, string) {
	rotating := IsAuto(model) // runnerWant asks it of a rotating agent only; ctx peeks unless the caller stamps the pick
	mq := QueryFor(ctx, ModelQuery{Agent: q.Agent, Model: model, Profile: profile, Task: q.Task, Default: q.AliasHint,
		Tools: q.Tools})
	// an untyped ask under an auto pin rotates at dispatch; a peek would pin the next pick without stamping it
	if !rotating && (IsAuto(mq.Model) || model == "" && IsAuto(mq.PinModel)) {
		return "", autoPickWhy
	}
	r, err := ResolveRun(reg, mq, time.Now())
	switch {
	case err != nil:
		return "", FallbackReason(err)
	case r.Model == "" || IsAuto(r.Model) || r.Source == "pool":
		return "", autoPickWhy
	}
	return r.Model, ""
}

// piChainMode is the mode a plain pi ask's chain follows, "" for one hop (a typed none or model).
func piChainMode(ctx context.Context, q RouteReq) string {
	plan, _ := PlanOf(ctx)
	oneHop := plan.Fallback.Kind == 0 || plan.Fallback.Kind == router.PolicyNone // an undecided policy fails closed
	if q.Model != "" || q.AutoModel || q.Resumed || q.Native || oneHop {
		return ""
	}
	return plan.Fallback.Mode()
}
