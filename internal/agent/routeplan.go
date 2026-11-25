package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// RunnerKind is the CLI a hop runs on.
type RunnerKind uint8

const (
	RunnerPi  RunnerKind = iota + 1 // Pi on the workspace, the asked agent's model
	RunnerOwn                       // the agent's own CLI
)

func (k RunnerKind) MarshalText() ([]byte, error) {
	switch k {
	case RunnerPi:
		return []byte("pi"), nil
	case RunnerOwn:
		return []byte("own"), nil
	}
	return nil, fmt.Errorf("runner kind %d", k)
}

func (k *RunnerKind) UnmarshalText(b []byte) error {
	switch string(b) {
	case "pi":
		*k = RunnerPi
	case "own":
		*k = RunnerOwn
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

// RoutePlan is the one routing decision of a dispatch; every reader takes it, none re-derives it.
type RoutePlan struct {
	Agent    string        `json:"agent"`
	Runners  []Runner      `json:"runners"`
	Pool     []Workspace   `json:"pool"`
	Tier     Tier          `json:"tier"`
	Fence    route.Fence   `json:"fence"`
	Fallback router.Policy `json:"fallback"`
	Barred   string        `json:"barred,omitempty"` // why Runners lacks pi, said once
	Agents   []PlanHop     `json:"agents,omitempty"` // the other-account and other-agent hops an agents or all policy adds
}

type planKey struct{}

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

// PlanRoute builds o's route plan from inputs only: it peeks, so no rotation pick is stamped.
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
	if p.Fence, err = planFence(ctx, parent); err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	if p.Fallback, err = planPolicy(ctx, o, ov.Fallback); err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	rctx, ro, _, err := rotateRole(ctx, o) // peeked: the pick the chain leads with, nothing stamped
	if err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	p.Pool = planPool(rctx, reg, ro, p) // before tier and runners: both read Pool[0]
	p.Tier = planTier(ctx, reg, o, p.Pool)
	p.Runners, p.Barred = planRunners(ctx, reg, o, p, parent.Carried || ov.Runner == agentconfig.RunnerOwn)
	if p.Agents, err = planAgentHops(rctx, ro, p); err != nil {
		return p, fmt.Errorf("route plan: %w", err)
	}
	return p, nil
}

// planAgentHops is the chain's own ladder tail under an agents or all policy: hops off the pool's accounts.
func planAgentHops(ctx context.Context, o RunOptions, p RoutePlan) ([]PlanHop, error) {
	if !p.Fallback.Agents || o.Host != "" {
		return nil, nil
	}
	eng, err := Lookup(o.Agent)
	if err != nil {
		return nil, err
	}
	chain, _, err := Resolve(ctx, RouteReq{Agent: o.Agent, Model: o.Model, AliasHint: HopAlias(o.Prompt, router.RuleClassifier{}.Alias),
		Task: o.Prompt, Profile: o.Provider, Lead: o.Lead, Fallback: p.Fallback.Mode(), WorkDir: o.WorkDir, Native: o.Native,
		Pinned: true, AutoModel: o.AutoModel, Tools: o.Tools, Engine: eng, Resumed: o.SessionID != "", Opts: o})
	if err != nil {
		return nil, err
	}
	var out []PlanHop
	for _, d := range chain.Dispatches {
		if d.runFor == "" && !slices.ContainsFunc(p.Pool, func(w Workspace) bool { return w.Account == d.Account }) {
			out = append(out, PlanHop{Runner: Runner{Kind: RunnerOwn, Agent: d.Agent.Name()}, Workspace: Workspace{Account: d.Account}})
		}
	}
	return out, nil
}

// planFence is the overlay's confinement held to the parent's fence.
func planFence(ctx context.Context, parent ParentRoute) (route.Fence, error) {
	profiles, open, err := proxy.ConfinementFence(ctx)
	if err != nil {
		return route.Fence{}, err
	}
	return parent.Fence.Narrow(route.Fence{Profiles: profiles, Open: open}), nil
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
		mode = router.FallbackAgents // nothing set anywhere: pi's own default
	}
	pol, err := router.PolicyOf(mode, src)
	if err != nil {
		return pol, err
	}
	if pol.Kind != router.PolicyNone && (router.ArmProfileContext(ctx) != "" || typedProfile(o) != "") {
		pol.Kind = router.PolicySameWorkspace
	}
	return pol, nil
}

// typedProfile is o's typed --provider profile, "" for none or the session lead.
func typedProfile(o RunOptions) string {
	if p := route.ParseLegacy(o.Provider).Profile; p != LeadSession {
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
	t.Note = fmt.Sprintf("%s pool empty for %s on %s, ran %s %s", rotationTiers[asked], o.Agent, p.Name, rotationTiers[alias], m)
	return t
}

// mayStepUp reports a fast or deep tier a rotating agent picks on a Databricks lead with no typed model.
func mayStepUp(reg *providers.Registry, o RunOptions, asked string, pool []Workspace) bool {
	untyped := o.Model == "" || IsAuto(o.Model)
	return asked != "" && asked != models.Sonnet && untyped && rotatesUnderAuto(o.Agent) &&
		len(pool) > 0 && databricksRow(reg, pool[0].Profile)
}

// planRunners is pi then the agent's own CLI on a Databricks lead, the own CLI alone with Barred when pi cannot run.
func planRunners(ctx context.Context, reg *providers.Registry, o RunOptions, p RoutePlan, own bool) ([]Runner, string) {
	self := Runner{Kind: RunnerOwn, Agent: p.Agent}
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

// piBar is why the pi runner cannot run o: --runner own, a resumed session, --native, a carried ask, a Pi gap.
func piBar(ctx context.Context, o RunOptions, agentName string, own bool) string {
	switch {
	case own || o.Runner == agentconfig.RunnerOwn:
		return "--runner own runs " + agentName + "'s own CLI"
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
	return append(rs, Runner{Kind: RunnerOwn, Agent: agentid.Claude})
}

// Hops is p runner-major (D3): pi on every pool workspace, each own CLI on Pool[0], then the agent hops.
func (p RoutePlan) Hops() []PlanHop {
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
	return append(out, p.Agents...)
}

// engineName is the CLI h launches, as a Dispatch's Agent.Name() spells it.
func (h PlanHop) engineName() string {
	if h.Runner.Kind == RunnerPi {
		return agentid.Pi
	}
	return h.Runner.Agent
}

// Divergence is one line per hop where p and ch differ in agent or account; nil when they agree.
func Divergence(p RoutePlan, ch Chain) []string {
	hops := p.Hops()
	var out []string
	for i := range max(len(hops), len(ch.Dispatches)) {
		plan, chain := "none", "none"
		if i < len(hops) {
			plan = hops[i].engineName() + "@" + cmp.Or(hops[i].Workspace.Account, hops[i].Workspace.Profile)
		}
		if i < len(ch.Dispatches) {
			chain = ch.Dispatches[i].Agent.Name() + "@" + ch.Dispatches[i].Account
		}
		if i < len(hops) && i < len(ch.Dispatches) && sameHop(hops[i], ch.Dispatches[i]) {
			continue
		}
		out = append(out, fmt.Sprintf("hop %d: plan %s, chain %s", i+1, plan, chain))
	}
	return out
}

// sameHop compares agent and account; an own-login workspace with no row compares by agent only.
func sameHop(h PlanHop, d Dispatch) bool {
	acct := h.Workspace.Account
	return h.engineName() == d.Agent.Name() && (acct == "" || acct == d.Account)
}
