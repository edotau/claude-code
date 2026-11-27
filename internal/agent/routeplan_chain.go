package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

// planChain fails closed on invalid planning inputs, resolves once, then records only admitted hops.
func (c *Client) planChain(ctx context.Context, r Engine, o RunOptions) (Chain, error) {
	p, err := PlanRoute(ctx, o, parentRoute(ctx))
	if err != nil {
		return Chain{}, err
	}
	ch, err := c.route(withPlan(ctx, p), r, o)
	if err != nil {
		ch.Plan = &p // the plan was built before the route failed: Prepare hands it back through PrepareError
		return ch, err
	}
	ch = withLocal(ch, p, o)
	p.Fallback = executablePolicy(p.Fallback)
	p = admittedPlan(p, ch)
	ch.Plan = &p
	return ch, nil
}

// admittedPlan records validation's result, not a second routing decision (including same-account agent tails).
func admittedPlan(p RoutePlan, ch Chain) RoutePlan {
	p.Admitted = make([]PlanHop, 0, len(ch.Dispatches))
	p.Agents = nil
	for i, d := range ch.Dispatches {
		h := PlanHop{Runner: Runner{Kind: RunnerCLI, Agent: d.Agent.Name()}, Workspace: admittedWorkspace(p, d)}
		if d.Agent.Name() == agentid.Pi {
			h.Runner = Runner{Kind: RunnerPi, Agent: d.agentName()}
		} else if i != ch.OwnHop() && !plannedRunnerHead(p, h) {
			p.Agents = append(p.Agents, h)
		}
		p.Admitted = append(p.Admitted, h)
	}
	hadPi := slices.ContainsFunc(p.Runners, func(r Runner) bool { return r.Kind == RunnerPi })
	p.Runners = slices.DeleteFunc(slices.Clone(p.Runners), func(r Runner) bool {
		return !slices.ContainsFunc(p.Admitted, func(h PlanHop) bool { return h.Runner == r })
	})
	if hadPi && ch.runners() == 0 && p.Agent != agentid.Pi && p.Barred == "" {
		p.Barred = piAdmissionReason(ch)
	}
	return p
}

// admittedWorkspace uses routing metadata, never a billing login misinterpreted as a profile.
func admittedWorkspace(p RoutePlan, d Dispatch) Workspace {
	for _, w := range p.Pool {
		if w.Account == d.Account {
			return w
		}
	}
	return Workspace{Account: d.Account, Profile: d.Route.Profile}
}

// piAdmissionReason preserves why a candidate Pi runner was removed during validation.
func piAdmissionReason(ch Chain) string {
	if ch.Preflight != nil {
		for _, note := range ch.Preflight.Notes {
			if _, why, ok := strings.Cut(note, ": pi runner skipped: "); ok {
				return why
			}
		}
	}
	return "the resolved lead is not the agent's Databricks hop"
}

// planOptions applies defaults and validates the caller's model before any routing pick.
func (c *Client) planOptions(ctx context.Context, o RunOptions) (RunOptions, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return o, err
	}
	if o.Host == "" {
		if o.Model, err = resolveRunModel(ctx, o); err != nil {
			return o, clientUsageError{fmt.Errorf("%w: %w", ErrModelRoute, err)}
		}
	}
	if err := refuseDirectContext(ctx, o.Direct); err != nil {
		return o, clientUsageError{err}
	}
	return o, nil
}

// planEngine makes the sole role rotation and chooses the local or remote engine.
func planEngine(ctx context.Context, o RunOptions) (context.Context, RunOptions, roleNote, Engine, error) {
	ctx, o, rotated, rerr := rotateRole(ctx, o)
	r, err := Lookup(o.Agent)
	if err = cmp.Or(rerr, err); err != nil {
		return ctx, o, rotated, nil, err
	}
	if o.Host != "" {
		r, err = hostEngine(o)
	}
	return ctx, o, rotated, r, err
}

// plannedRunnerHead reports a runner's lead-workspace hop, not a same-agent fallback on another account.
func plannedRunnerHead(p RoutePlan, h PlanHop) bool {
	return slices.Contains(p.Runners, h.Runner) && len(p.Pool) > 0 && p.Pool[0].Account == h.Workspace.Account
}

// executablePolicy records the legacy agents/all ladder's domain, while Pool still bounds Pi's workspaces.
func executablePolicy(p router.Policy) router.Policy {
	if p.Kind == router.PolicySameWorkspace && p.Agents {
		p.Kind = router.PolicyAnyInFence
	}
	return p
}

// routeRequestKey carries Resolve's full ask into the candidate admission pass, including previews.
type routeRequestKey struct{}

// plannedChain emits runner hops before admitting the first own CLI, never splicing a completed fallback chain.
type plannedChain struct {
	ctx       context.Context
	q         RouteReq
	reg       *providers.Registry
	checked   map[string]*Finding
	plain     []Dispatch
	plainMode string
	backstop  Engine
}

func newPlannedChain(ctx context.Context, reg *providers.Registry) *plannedChain {
	q, _ := ctx.Value(routeRequestKey{}).(RouteReq)
	return &plannedChain{ctx: ctx, q: q, reg: reg, checked: make(map[string]*Finding)}
}

// admit checks each source CLI once, before choosing the lead whose model the Pi runner will execute.
func (p *plannedChain) admit(c Chain, d Dispatch, pf *Preflight) Chain {
	name := d.Agent.Name()
	f, checked := p.checked[name]
	if !checked {
		f = cliVersionFinding(p.ctx, name, pf)
		p.checked[name] = f
		if f != nil {
			c.Skipped = append(c.Skipped, f.Detail)
		}
	}
	if f != nil {
		return c
	}
	return p.append(c, d)
}

// append assembles the lead's runners once; subsequent validated candidates retain their original order.
func (p *plannedChain) append(c Chain, d Dispatch) Chain {
	if len(c.Dispatches) == 0 && len(p.plain) == 0 && p.q.Agent != "" {
		if p.startPlain(d) {
			p.plain = append(p.plain, d)
			return c
		}
		plan, _ := PlanOf(p.ctx)
		if p.q.Agent != agentid.Pi {
			hops, notes := plan.runnerHops(p.ctx, p.reg, p.q, d)
			c.Dispatches = append(c.Dispatches, hops...)
			c.Dispatches = append(c.Dispatches, d)
			return planNotes(c, notes)
		}
	}
	if len(p.plain) > 0 && p.plainMode == route.FallbackPin && d.Agent.Name() == agentid.Pi && d.Account != p.plain[0].Account {
		return c
	}
	if len(p.plain) > 0 && len(c.Dispatches) == 0 && d.Agent.Name() == agentid.Pi {
		p.plain = append(p.plain, d)
		return c
	}
	c = p.finish(c)
	c.Dispatches = append(c.Dispatches, d)
	return c
}

func planNotes(c Chain, notes []string) Chain {
	for _, note := range notes {
		c = c.noted(note)
	}
	return c
}

// startPlain buffers only a plain Pi ask's leading Pi candidates, so existing siblings keep their metadata.
func (p *plannedChain) startPlain(d Dispatch) bool {
	if p.q.Agent != agentid.Pi || !ownDatabricksHop(p.reg, p.q, d) {
		return false
	}
	p.plainMode = piChainMode(p.ctx, p.q)
	var err error
	p.backstop, err = Lookup(agentid.Claude)
	return p.plainMode != "" && err == nil
}

// finish emits a plain Pi ask's prefix once, before its first non-Pi candidate or the end of admission.
func (p *plannedChain) finish(c Chain) Chain {
	if len(p.plain) == 0 {
		return c
	}
	defer func() { p.plain = nil }()
	if len(c.Dispatches) > 0 {
		return c
	}
	lead := p.plain[0]
	c.Dispatches = append(c.Dispatches, lead)
	if p.plainMode != route.FallbackPin && lead.Model != "" && !IsAuto(lead.Model) {
		c = p.plainSiblings(c, lead)
	}
	c.Dispatches = append(c.Dispatches, p.plain[1:]...)
	_, profile, _ := strings.Cut(lead.Account, "·")
	alias := cmp.Or(p.q.AliasHint, models.Sonnet)
	q := RouteReq{Agent: agentid.Claude, AliasHint: alias, Tools: p.q.Tools}
	if _, why := agentModelOn(PeekContext(p.ctx), p.reg, q, alias, profile); why != "" {
		return c.noted("claude fallback hop skipped: " + why)
	}
	c.Dispatches = append(c.Dispatches, Dispatch{Agent: p.backstop, Model: alias, Route: rowRoute(p.reg, profile),
		Account: lead.Account, Billing: lead.Billing, otherRoute: true, fixedModel: true})
	return c
}

func (p *plannedChain) plainSiblings(c Chain, lead Dispatch) Chain {
	sibs, skips, why := piSiblings(p.ctx, p.reg, lead, lead.Model)
	if why != "" {
		return c.noted("pi sibling hops skipped: " + why)
	}
	for _, skip := range skips {
		c = c.noted("pi sibling hop skipped: " + skip)
	}
	for _, d := range sibs {
		if slices.ContainsFunc(p.plain, func(e Dispatch) bool { return e.Agent == lead.Agent && e.Account == d.Account }) {
			continue
		}
		d.Agent = lead.Agent
		c.Dispatches = append(c.Dispatches, d)
	}
	return c
}

// runnerCandidates counts the extra candidates previewOutcome projected from Pi runner metadata.
func (c Chain) runnerCandidates() int {
	n := 0
	for _, d := range c.Dispatches {
		if d.runFor != "" && d.rung > 0 {
			n++
		}
	}
	return n
}

func (p *plannedChain) contains(c Chain, d Dispatch) bool {
	return slices.ContainsFunc(c.Dispatches, d.same) || slices.ContainsFunc(p.plain, d.same)
}
