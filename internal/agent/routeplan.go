package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

// RunnerKind is the CLI a hop runs on.
type RunnerKind uint8

const (
	RunnerPi  RunnerKind = iota + 1 // Pi on the workspace, the asked agent's model
	RunnerCLI                       // the agent's own CLI
)

func (k RunnerKind) MarshalText() ([]byte, error) {
	switch k {
	case RunnerPi:
		return []byte("pi"), nil
	case RunnerCLI:
		return []byte("own"), nil
	}
	return nil, fmt.Errorf("runner kind %d", k)
}

func (k *RunnerKind) UnmarshalText(b []byte) error {
	switch string(b) {
	case "pi":
		*k = RunnerPi
	case "own":
		*k = RunnerCLI
	default:
		return fmt.Errorf("runner kind %q", b)
	}
	return nil
}

// Runner is one CLI a hop can run on; Agent is the asked agent, or claude for a cross-agent backstop.
type Runner struct {
	Kind  RunnerKind `json:"kind"`
	Agent string     `json:"agent"`
}

// Workspace is one row the plan may run on, Picked for the auto pick or typed start.
type Workspace struct {
	Profile string `json:"profile"`
	Account string `json:"account"`
	Picked  bool   `json:"picked,omitempty"`
}

// Tier is decided once: the brief's tier, the alias hops run, and the one step-up note.
type Tier struct {
	Asked, Signal, Alias, Note string
}

// ParentRoute is what a dispatch inherits: the fence, a carried child, a --retry record's plan.
type ParentRoute struct {
	Fence   route.Fence
	Carried bool
	Replay  *RoutePlan
}

// parentRoute is what a dispatch inherits: an open fence, or a --retry plan's, and whether it is a carried child.
func parentRoute(ctx context.Context) ParentRoute {
	pr := ParentRoute{Fence: route.Fence{Open: true}, Carried: carriedChild(ctx, "")}
	if r, ok := RetriedOf(ctx); ok && r.Plan != nil {
		pr.Replay = r.Plan
		if f := r.Plan.Fence; r.Plan.Fenced || f.Open || len(f.Profiles) > 0 { // an old plan with a zero fence lacked one
			pr.Fence = f
		}
	}
	return pr
}

// RoutePlan is the one routing decision of a dispatch; every reader takes it, none re-derives it.
type RoutePlan struct {
	Agent    string        `json:"agent"`
	Runners  []Runner      `json:"runners"`
	Pool     []Workspace   `json:"pool"`
	Tier     Tier          `json:"tier"`
	Fence    route.Fence   `json:"fence"`
	Fenced   bool          `json:"fenced,omitempty"` // Fence was decided: a closed, empty one is not a plan that lacked it
	Fallback router.Policy `json:"fallback"`
	Barred   string        `json:"barred,omitempty"`   // why Runners lacks pi, said once
	Admitted []PlanHop     `json:"admitted,omitempty"` // validated execution steps; nil before the single routing pass
	Agents   []PlanHop     `json:"agents,omitempty"`   // the other-account and other-agent hops an agents or all policy adds
	Local    *LocalHop     `json:"local,omitempty"`    // the last-resort row, run after every other hop at stage last
}

type planKey struct{}

type retryOfKey struct{}

// Retried is the run a --retry replays: its dispatch id and the route plan to replay (nil for an old record).
type Retried struct {
	ID   string
	Plan *RoutePlan
}

// WithRetried marks ctx's ask as a --retry of r; parentRoute replays r.Plan.
func WithRetried(ctx context.Context, r Retried) context.Context {
	return context.WithValue(ctx, retryOfKey{}, r)
}

// RetriedOf is the run ctx's ask retries; false for a fresh ask.
func RetriedOf(ctx context.Context) (Retried, bool) {
	r, ok := ctx.Value(retryOfKey{}).(Retried)
	return r, ok
}

// withPlan carries p to every reader below Client.Plan.
func withPlan(ctx context.Context, p RoutePlan) context.Context {
	return context.WithValue(ctx, planKey{}, p)
}

// PlanOf is the route plan Client.Plan built for ctx's ask; false for a path with no Client.Plan.
func PlanOf(ctx context.Context) (RoutePlan, bool) {
	p, ok := ctx.Value(planKey{}).(RoutePlan)
	return p, ok
}

// PlanHop is one planned step: a runner on a workspace.
type PlanHop struct {
	Runner    Runner
	Workspace Workspace
}

// PlanRoute builds from resolved inputs only; Client.Plan owns the single role rotation.
func PlanRoute(ctx context.Context, o RunOptions, parent ParentRoute) (RoutePlan, error) {
	ctx = PeekContext(ctx)
	p := RoutePlan{Agent: cmp.Or(o.Agent, agentid.Claude)}
	reg, err := providers.Load()
	if err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	ov, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	if p.Fence, p.Fallback, err = planDecisions(ctx, o, parent, ov); err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	p.Fenced = true
	p.Pool = leadWith(planPool(ctx, reg, o, p), replayLead(parent.Replay)) // before tier and runners: both read Pool[0]
	p.Tier = planTier(ctx, reg, o, p.Pool)
	p.Runners, p.Barred = planRunners(ctx, reg, o, p, parent.Carried || ov.Runner == route.RunnerCLI)
	if rp := parent.Replay; rp != nil && len(rp.Runners) > 0 && rp.Agent == p.Agent && o.SessionID == "" {
		p.Runners, p.Barred = slices.Clone(rp.Runners), rp.Barred // a retry replays its runners; a session stays on its CLI
	}
	p.Local = planLocal(ctx, reg, o, p)
	return p, nil
}

// PreviewRoute is Resolve for a no-run preview under the plan a dispatch of q builds; the chain carries it.
func PreviewRoute(ctx context.Context, q RouteReq) (Chain, router.PickResult, error) {
	ctx = PeekContext(ctx)
	o := q.Opts
	o.Agent, o.Model, o.Prompt, o.Lead, o.Fallback = q.Agent, q.Model, cmp.Or(o.Prompt, q.Task), q.Lead, q.Fallback
	o.Provider, o.Native, o.AutoModel, o.Tools = cmp.Or(o.Provider, q.Profile), q.Native, q.AutoModel, q.Tools
	if o.Lead == "" && q.Profile == "" && q.Provider != "" { // a provider-only preview plans on the lead Resolve picks
		if lead, _, err := leadForContext(ctx, q); err == nil {
			o.Lead = lead
		}
	}
	var pick router.PickResult
	ch, _, err := planAndResolve(ctx, o, func(ctx context.Context) (ch Chain, err error) {
		ch, pick, err = Resolve(ctx, q)
		return ch, err
	})
	return ch, pick, err
}

// planAndResolve builds the plan for o, resolves under it and attaches it to the chain, also when resolve fails.
func planAndResolve(ctx context.Context, o RunOptions, resolve func(context.Context) (Chain, error)) (Chain, RoutePlan, error) {
	p, err := PlanRoute(ctx, o, parentRoute(ctx))
	if err != nil {
		return Chain{}, p, err
	}
	ch, err := resolve(withPlan(ctx, p))
	ch.Plan = &p // Prepare hands the plan back through PrepareError when the route failed
	return ch, p, err
}

// replayLead is the workspace a replayed plan ran first: its first admitted hop's, else its pool head.
func replayLead(rp *RoutePlan) string {
	switch {
	case rp == nil:
		return ""
	case len(rp.Admitted) > 0:
		return rp.Admitted[0].Workspace.Profile
	case len(rp.Pool) > 0:
		return rp.Pool[0].Profile
	}
	return ""
}

// leadWith moves profile to the head of pool when pool holds it; it never adds a workspace the plan barred.
func leadWith(pool []Workspace, profile string) []Workspace {
	i := slices.IndexFunc(pool, func(w Workspace) bool { return w.Profile == profile })
	if i <= 0 {
		return pool
	}
	out := append([]Workspace{pool[i]}, pool[:i]...)
	return append(out, pool[i+1:]...)
}

// planDecisions is the one fence and policy decision, read by the rotation's pre-pass and by PlanRoute alike.
func planDecisions(ctx context.Context, o RunOptions, parent ParentRoute, ov agentconfig.Overlay) (route.Fence, router.Policy, error) {
	f, err := planFence(ctx, parent, ov.Fence)
	if err != nil {
		return f, router.Policy{}, err
	}
	pol, err := planPolicy(ctx, o, ov.Fallback)
	if rp := parent.Replay; err == nil && rp != nil && rp.Fallback.Kind != 0 && o.SessionID == "" {
		pol = rp.Fallback // a retry keeps the policy it ran under; a resumed session stays put
	}
	return f, pol, err
}

// planFence is the overlay's confinement and its fence key held to the parent's fence.
func planFence(ctx context.Context, parent ParentRoute, key *route.Fence) (route.Fence, error) {
	f, err := proxy.ConfinementFence(ctx)
	if err != nil {
		return route.Fence{}, err
	}
	f = parent.Fence.Narrow(f)
	if key != nil {
		f = f.Narrow(*key)
	}
	return f, nil
}

// planPolicy is the typed mode, else the configured one; a session is none, a confinement or pin one workspace.
func planPolicy(ctx context.Context, o RunOptions, overlayMode string) (router.Policy, error) {
	if o.SessionID != "" {
		return router.Policy{Kind: router.PolicyNone, Source: router.PolicyTyped}, nil
	}
	mode, src := o.Fallback, router.PolicyTyped
	if o.FallbackDefaulted || o.Fallback == "" {
		mode, src = router.FallbackSetting(ctx, overlayMode)
	}
	if o.Agent == agentid.Pi && src == router.PolicyDefault {
		mode = route.FallbackAgents // nothing set anywhere: pi's own default
	}
	pol, err := router.PolicyOf(mode, src)
	if err != nil {
		return pol, err
	}
	if pol.Kind != router.PolicyNone && (cmp.Or(router.ArmProfileContext(ctx), typedProfile(o)) != "") {
		pol.Kind = router.PolicySameWorkspace
	}
	return pol, nil
}

// typedProfile is o's typed --provider profile, "" for none or the session lead.
func typedProfile(o RunOptions) string {
	if p := route.FromWord(o.Provider).Profile; p != LeadSession {
		return p
	}
	return ""
}

// planPool is the lead workspace, then the fence's sibling workspaces when the policy may move.
func planPool(ctx context.Context, reg *providers.Registry, o RunOptions, p RoutePlan) []Workspace {
	head := planLead(ctx, reg, o)
	if head == "" {
		return nil
	}
	pool := []Workspace{{Profile: head, Account: workspaceAccount(reg, head), Picked: true}}
	if p.Fallback.Kind != router.PolicyAnyInFence || agentid.Routing(p.Agent).NoWorkspaceFallback || !databricksRow(reg, head) {
		return pool
	}
	drop := router.RotatedLeadOf(ctx).Drop
	for _, ws := range router.WorkspaceRows(reg, head) {
		if p.Fence.Allows(ws) && !slices.Contains(drop, ws) {
			pool = append(pool, Workspace{Profile: ws, Account: workspaceAccount(reg, ws)})
		}
	}
	return pool
}

// planLead is the confined workspace, else the typed profile or lead, the agent's record pin, the session lead.
func planLead(ctx context.Context, reg *providers.Registry, o RunOptions) string {
	if arm := router.ArmProfileContext(ctx); arm != "" {
		return arm
	}
	if p := cmp.Or(typedProfile(o), o.Lead); p != "" {
		return p
	}
	if !VaultAgentIsClaude(o.Agent) {
		if pin := pinProfile(vault.GetAgentRecord(o.Agent)); pin != "" && pin != PoolPin {
			return pin
		}
	}
	return providers.SessionLeadContext(ctx, reg, true)
}

// workspaceAccount is profile's billing account, "" for an own-login pin with no registry row.
func workspaceAccount(reg *providers.Registry, profile string) string {
	if p := reg.Row(profile); p != nil {
		return router.AccountOf(p)
	}
	return ""
}

func databricksRow(reg *providers.Registry, profile string) bool {
	p := reg.Row(profile)
	return p != nil && p.Preset == providers.PresetDatabricks
}

// planTier is the brief's tier and, for an untyped model on a Databricks lead, the rotation's step-up.
func planTier(ctx context.Context, reg *providers.Registry, o RunOptions, pool []Workspace) Tier {
	asked, signal := TaskTier(o.Prompt)
	t := Tier{Asked: asked, Signal: signal, Alias: asked}
	if !mayStepUp(reg, o, asked, pool) {
		return t
	}
	p := reg.Row(pool[0].Profile)
	m, _, alias, err := tierRotation(QueryFor(ctx, ModelQuery{Agent: o.Agent, Tools: o.Tools}), asked, p, time.Now())
	if err != nil || m == "" || alias == asked {
		return t
	}
	t.Alias = alias
	t.Note = poolEmptyNote(asked, o.Agent, p, alias, m)
	return t
}

// poolEmptyNote is the one step-up line: the asked tier's pool is empty on p, so model m ran at alias's tier.
func poolEmptyNote(asked, agentName string, p *providers.Provider, alias, m string) string {
	return fmt.Sprintf("%s pool empty for %s on %s, ran %s %s", rotationTiers[asked], agentName, p.Name, rotationTiers[alias], m)
}

// tierAlias is the plan's tier alias, else the brief's own (previews and hooks have no plan).
func tierAlias(ctx context.Context, prompt string) string {
	if ctx != nil {
		if plan, ok := PlanOf(ctx); ok {
			return plan.Tier.Alias
		}
	}
	alias, _ := TaskTier(prompt)
	return alias
}

// plannedStepUp is the plan's tier when it stepped up for task's brief; false otherwise.
func plannedStepUp(ctx context.Context, task string) (Tier, bool) {
	if ctx == nil {
		return Tier{}, false
	}
	plan, ok := PlanOf(ctx)
	asked, _ := TaskTier(task)
	return plan.Tier, ok && plan.Tier.Note != "" && plan.Tier.Asked == asked
}

// decidedFence is the plan's fence and policy, else the pre-pass's: Client.Plan rotates before PlanRoute runs.
func decidedFence(ctx context.Context, o RunOptions) (RoutePlan, error) {
	if plan, ok := PlanOf(ctx); ok {
		return plan, nil
	}
	return preFence(ctx, o)
}

// preFence is the fence and policy before the rotation pick, which reads them through fenced; an error refuses.
func preFence(ctx context.Context, o RunOptions) (RoutePlan, error) {
	ctx = PeekContext(ctx)
	ov, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil {
		return RoutePlan{}, fmt.Errorf("route plan: %w", err)
	}
	var p RoutePlan
	if p.Fence, p.Fallback, err = planDecisions(ctx, o, parentRoute(ctx), ov); err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	p.Fallback = executablePolicy(p.Fallback)
	return p, nil
}

// mayStepUp reports a fast or deep tier a rotating agent picks on a Databricks lead with no typed model.
func mayStepUp(reg *providers.Registry, o RunOptions, asked string, pool []Workspace) bool {
	untyped := o.Model == "" || IsAuto(o.Model)
	return asked != "" && asked != models.Sonnet && untyped && rotatesUnderAuto(o.Agent) &&
		len(pool) > 0 && databricksRow(reg, pool[0].Profile)
}

// planRunners is pi then the agent's own CLI on a Databricks lead, the own CLI alone with Barred when pi cannot run.
func planRunners(ctx context.Context, reg *providers.Registry, o RunOptions, p RoutePlan, own bool) ([]Runner, string) {
	self := Runner{Kind: RunnerCLI, Agent: p.Agent}
	onDatabricks := len(p.Pool) > 0 && databricksRow(reg, p.Pool[0].Profile)
	if p.Agent == agentid.Pi {
		return piAskRunners(o, p, onDatabricks), ""
	}
	if !agentid.Known(p.Agent) || !onDatabricks {
		return []Runner{self}, ""
	}
	if why := piBar(ctx, o, p.Agent, own); why != "" {
		return []Runner{self}, why
	}
	return []Runner{{Kind: RunnerPi, Agent: p.Agent}, self}, "" // --fallback none never drops pi (D2)
}

// piBar is why the pi runner cannot run o: --runner cli, a resumed session, --native, a carried ask, a Pi gap.
func piBar(ctx context.Context, o RunOptions, agentName string, own bool) string {
	switch {
	case own || o.Runner == route.RunnerCLI:
		return "--runner cli runs " + agentName + "'s own CLI"
	case o.SessionID != "":
		return "a resumed session stays on its own CLI"
	case o.Native:
		return "--native runs the agent's own config"
	case carriedChild(ctx, agentName):
		return "a carried ask runs " + agentName + "'s own CLI"
	}
	engine, err := Lookup(agentid.Pi)
	if err != nil { // a build without the driver: install state, like Available, is the launch's check
		return ""
	}
	return runnerLoses(engine.Caps(), o)
}

// piAskRunners is a plain pi ask: pi, then claude on the Databricks lead unless one hop was asked (piChainMode).
func piAskRunners(o RunOptions, p RoutePlan, onDatabricks bool) []Runner {
	rs := []Runner{{Kind: RunnerPi, Agent: agentid.Pi}}
	oneHop := o.Model != "" || o.AutoModel || o.SessionID != "" || o.Native
	moves := p.Fallback.Kind != router.PolicyNone || p.Fallback.Source == router.PolicyDefault
	if oneHop || !moves || !onDatabricks {
		return rs
	}
	return append(rs, Runner{Kind: RunnerCLI, Agent: agentid.Claude})
}

// Hops is p runner-major (D3): pi on every pool workspace, each own CLI on Pool[0], the agent hops, then local.
func (p RoutePlan) Hops() []PlanHop {
	if p.Admitted != nil {
		return p.Admitted
	}
	var out []PlanHop
	for _, r := range p.Runners {
		ws := p.Pool[:min(1, len(p.Pool))]
		if r.Kind == RunnerPi {
			ws = p.Pool
		}
		for _, w := range ws {
			out = append(out, PlanHop{Runner: r, Workspace: w})
		}
	}
	out = append(out, p.Agents...)
	if h, ok := p.Local.hop(p.Agent); ok {
		out = append(out, h)
	}
	return out
}

// runnerHops validates the plan's Pi prefix using the admitted lead, without building an own-CLI chain first.
func (p RoutePlan) runnerHops(ctx context.Context, reg *providers.Registry, q RouteReq, lead Dispatch) ([]Dispatch, []string) {
	if len(p.Runners) == 0 || p.Runners[0].Kind != RunnerPi {
		if p.Barred != "" && !strings.HasPrefix(p.Barred, "--runner cli") {
			return nil, []string{q.Agent + ": pi runner skipped: " + p.Barred + "; runs on " + q.Agent + "'s own CLI"}
		}
		return nil, nil
	}
	if !ownDatabricksHop(reg, q, lead) {
		return nil, nil
	}
	pi, err := Lookup(agentid.Pi)
	if err != nil || pi.Available() != nil {
		return nil, []string{q.Agent + ": pi runner skipped: pi is not installed"}
	}
	hops, skips, why := piRunnerHops(ctx, reg, q, lead, pi)
	if why != "" {
		return nil, []string{q.Agent + ": pi runner skipped: " + why}
	}
	var notes []string
	for _, skip := range skips {
		notes = append(notes, q.Agent+": pi runner hop skipped: "+skip)
	}
	return hops, notes
}

// planChain fails closed on invalid planning inputs, resolves once, then records only admitted hops.
func (c *Client) planChain(ctx context.Context, r Engine, o RunOptions) (Chain, error) {
	ch, p, err := planAndResolve(ctx, o, func(ctx context.Context) (Chain, error) { return c.route(ctx, r, o) })
	if err != nil {
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
