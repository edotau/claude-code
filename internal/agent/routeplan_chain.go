package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/router"
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
	p.Fallback = executablePolicy(p.Fallback)
	p.Constraints = planConstraints(p, ch)
	logDivergence(ctx, o.Agent, p.Constraints) // expected validation drops and ladder tails are not divergences
	p = admittedPlan(p, ch)
	ch.Plan = &p
	return ch, nil
}

// admittedPlan records validation's result, not a second routing decision (including same-account agent tails).
func admittedPlan(p RoutePlan, ch Chain) RoutePlan {
	p.Admitted = make([]PlanHop, 0, len(ch.Dispatches))
	p.Agents = nil
	for i, d := range ch.Dispatches {
		h := PlanHop{Runner: Runner{Kind: RunnerOwn, Agent: d.Agent.Name()}, Workspace: admittedWorkspace(p, d)}
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

// planConstraints compares admitted hops to independent policy and runner intent, not to an echoed hop list.
func planConstraints(p RoutePlan, ch Chain) []string {
	var out []string
	for i, d := range ch.Dispatches {
		if d.Agent.Name() == agentid.Pi && !slices.ContainsFunc(p.Runners, func(r Runner) bool { return r.Kind == RunnerPi }) {
			out = append(out, fmt.Sprintf("hop %d: pi is not a planned runner", i+1))
		}
		if p.Fallback.Kind == router.PolicyNone || p.Fallback.Kind == router.PolicySameWorkspace {
			if len(p.Pool) > 0 && p.Pool[0].Account != "" && d.Account != p.Pool[0].Account {
				out = append(out, fmt.Sprintf("hop %d: account %s leaves planned workspace %s", i+1, d.Account, p.Pool[0].Account))
			}
		}
	}
	return out
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
