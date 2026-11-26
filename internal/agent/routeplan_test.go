package agent

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

const devArm = `{"provider":"databricks","profile":"dev"}`

// planRouteRow is one routing rule PlanRoute decides once; the session profile is dev unless session names one.
type planRouteRow struct {
	name, overlay, session string // overlay is COWORKER_OVERLAY
	setup                  func(t *testing.T)
	o                      RunOptions
	check                  func(t *testing.T, p RoutePlan)
}

// planRouteRows: the plan's nine rows (2026-11-25-route-plan, Phase 2.1) plus the two baseline inconsistencies.
var planRouteRows = []planRouteRow{
	{name: "auto-pick keeps pi", overlay: devArm, session: "prod", o: RunOptions{Agent: "codex", Prompt: "Reply ok"}, check: checkAutoPick},
	{name: "copilot pin kept", setup: func(t *testing.T) { activateAgents(t, "copilot") },
		o: RunOptions{Agent: "copilot", Prompt: "Reply ok"}, check: checkCopilotPin},
	{name: "child fence", overlay: devArm, o: RunOptions{Agent: "opencode", Prompt: "Reply ok", Fallback: "all"},
		check: checkChildFence},
	{name: "gemini route token", o: RunOptions{Agent: "gemini", Prompt: "Reply ok", Provider: "dev"}, check: checkGeminiDev},
	{name: "codex fast step-up", setup: func(t *testing.T) { saveListing(t, "dev", []string{"databricks-gpt-6-1-sol"}) },
		o: RunOptions{Agent: "codex", Prompt: "Tier: fast\nRename foo to bar in /tmp/x.go"}, check: checkFastStepUp},
	{name: "runner own", o: RunOptions{Agent: "codex", Prompt: "Reply ok", Runner: "cli"}, check: checkRunnerCLI},
	{name: "typed none keeps pi", o: RunOptions{Agent: "codex", Prompt: "Reply ok", Fallback: "none"}, check: checkTypedNone},
	{name: "overlay none is a request", overlay: `{"fallback":"none"}`, o: RunOptions{Agent: "codex", Prompt: "Reply ok"},
		check: checkOverlayNone},
	{name: "resumed session", o: RunOptions{Agent: "codex", Prompt: "Reply ok", SessionID: "s-1", Fallback: "all"},
		check: checkResumed},
	{name: "resolve and dry-run share one workspace", o: RunOptions{Agent: "codex"}, check: checkResolveWorkspace},
	{name: "untyped claude pool leads with the rotation pick", session: "prod",
		o: RunOptions{Agent: "claude", Prompt: "Reply ok", Fallback: "all"}, check: checkRotationLead},
	{name: "standard tier carries no deep alias", o: RunOptions{Agent: "codex", Prompt: "Reply ok"}, check: checkStandardAlias},
}

func TestPlanRoute(t *testing.T) {
	for _, r := range planRouteRows {
		t.Run(r.name, func(t *testing.T) {
			autoFixture(t)
			if r.setup != nil {
				r.setup(t)
			}
			session := cmp.Or(r.session, "dev")
			ctx := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: session, route.OverlayVar: r.overlay})
			ctx, o, _, err := rotateRole(PeekContext(ctx), r.o)
			if err != nil {
				t.Fatal(err)
			}
			p, err := PlanRoute(ctx, o, ParentRoute{Fence: route.Fence{Open: true}})
			if err != nil {
				t.Fatal(err)
			}
			r.check(t, p)
		})
	}
}

func checkAutoPick(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerPi, RunnerCLI)
	if len(p.Pool) == 0 || p.Pool[0].Profile != "dev" || !p.Pool[0].Picked {
		t.Errorf("pool = %+v, want dev picked first", p.Pool)
	}
	if p.Fallback.Kind != router.PolicyNone || p.Fallback.Source != router.PolicyDefault {
		t.Errorf("fallback = %+v, want none from the default", p.Fallback)
	}
}

func checkRotationLead(t *testing.T, p RoutePlan) {
	if len(p.Pool) == 0 || p.Pool[0].Profile == "prod" {
		t.Errorf("pool = %+v, want the rotation pick to lead, not the session profile prod", p.Pool)
	}
}

func checkCopilotPin(t *testing.T, p RoutePlan) {
	wantPool(t, p, "personal")
	wantRunners(t, p, RunnerCLI)
}

func checkChildFence(t *testing.T, p RoutePlan) {
	if p.Fence.Allows("prod") || p.Fence.Allows("qa") {
		t.Errorf("fence %+v admits a sibling workspace", p.Fence)
	}
	wantPool(t, p, "dev")
	if !p.Fallback.OffArm || p.Fallback.Source != router.PolicyTyped || p.Fallback.Kind != router.PolicySameWorkspace {
		t.Errorf("fallback = %+v, want typed all held to the confined workspace", p.Fallback)
	}
	for _, h := range p.Hops() {
		if h.Workspace.Profile != "" && !p.Fence.Allows(h.Workspace.Profile) { // agent hops: fence enforcement is Phase 5.1
			t.Errorf("hop %+v leaves the fence", h)
		}
	}
}

func checkGeminiDev(t *testing.T, p RoutePlan) {
	wantPool(t, p, "dev")
	if !p.Fence.Allows("dev") {
		t.Errorf("fence %+v refuses dev", p.Fence)
	}
}

func checkFastStepUp(t *testing.T, p RoutePlan) {
	if p.Tier.Asked != models.Haiku || p.Tier.Alias != models.Sonnet || p.Tier.Note == "" {
		t.Errorf("tier = %+v, want haiku stepped to sonnet with one note", p.Tier)
	}
}

func checkRunnerCLI(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerCLI)
	if !strings.Contains(p.Barred, "--runner cli") {
		t.Errorf("barred = %q, want it to name --runner cli", p.Barred)
	}
}

func checkTypedNone(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerPi, RunnerCLI)
	if p.Fallback.Source != router.PolicyTyped || len(p.Pool) != 1 {
		t.Errorf("fallback %+v pool %+v, want typed none on one workspace", p.Fallback, p.Pool)
	}
}

func checkOverlayNone(t *testing.T, p RoutePlan) {
	if p.Fallback.Source != router.PolicyOverlay || !p.Fallback.Typed() {
		t.Errorf("fallback = %+v, want a typed overlay none", p.Fallback)
	}
}

func checkResumed(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerCLI)
	if p.Fallback.Kind != router.PolicyNone {
		t.Errorf("fallback = %+v, want none", p.Fallback)
	}
}

// checkResolveWorkspace: baseline resolve picked prod/deep where dry-runs pick dev; the plan has one lead, no tier.
func checkResolveWorkspace(t *testing.T, p RoutePlan) {
	wantPool(t, p, "dev")
	if p.Tier != (Tier{}) {
		t.Errorf("tier = %+v, want none for no brief", p.Tier)
	}
}

// checkStandardAlias: baseline noted codex's opus alias while the hop ran the standard model.
func checkStandardAlias(t *testing.T, p RoutePlan) {
	if p.Tier.Asked != models.Sonnet || p.Tier.Alias != models.Sonnet || strings.Contains(p.Tier.Note, models.Opus) {
		t.Errorf("tier = %+v, want sonnet with no opus alias note", p.Tier)
	}
}

// planChainOf is a chain of "<engine>@<account>" hops.
func planChainOf(hops ...string) Chain {
	var c Chain
	for _, h := range hops {
		name, acct, _ := strings.Cut(h, "@")
		c.Dispatches = append(c.Dispatches, Dispatch{Agent: &handoffRunner{name: name}, Account: acct})
	}
	return c
}

// wantRunners checks p's runner kinds in order.
func wantRunners(t *testing.T, p RoutePlan, kinds ...RunnerKind) {
	t.Helper()
	got := make([]RunnerKind, 0, len(p.Runners))
	for _, r := range p.Runners {
		got = append(got, r.Kind)
	}
	if !slices.Equal(got, kinds) {
		t.Errorf("runners = %+v (barred %q), want kinds %v", p.Runners, p.Barred, kinds)
	}
}

func wantPool(t *testing.T, p RoutePlan, profiles ...string) {
	t.Helper()
	got := make([]string, 0, len(p.Pool))
	for _, w := range p.Pool {
		got = append(got, w.Profile)
	}
	if !slices.Equal(got, profiles) {
		t.Errorf("pool = %v, want %v", got, profiles)
	}
}

// Without a plan in ctx, tierAlias is the brief's own TaskTier alias.
func TestTierAliasNoPlanMatchesTaskTier(t *testing.T) {
	for _, prompt := range []string{"Tier: fast\nRename foo to bar", "Reply ok"} {
		want, _ := TaskTier(prompt)
		if got := tierAlias(context.Background(), prompt); got != want {
			t.Errorf("tierAlias(%q) = %q, want %q", prompt, got, want)
		}
	}
}

// A workspace pin under a typed all still walks the legacy agent ladder: the recorded plan says any-in-fence.
func TestExecutablePolicyPinnedAll(t *testing.T) {
	got := executablePolicy(router.Policy{Kind: router.PolicySameWorkspace, Agents: true, OffArm: true, Source: router.PolicyTyped})
	if got.Kind != router.PolicyAnyInFence || !got.OffArm {
		t.Errorf("executablePolicy = %+v, want any-in-fence with off-arm", got)
	}
}

func TestFinishPrepareRecordsAdmittedPlan(t *testing.T) {
	pol, err := router.PolicyOf(route.FallbackAgents, router.PolicyTyped)
	if err != nil {
		t.Fatal(err)
	}
	plan := &RoutePlan{Agent: "codex", Runners: []Runner{{RunnerPi, "codex"}, {RunnerCLI, "codex"}}, Fallback: pol}
	c := &Client{}
	p := &Prepared{Chain: Chain{Plan: plan}}
	if err := c.finishPrepare(context.Background(), p, RunOptions{Agent: "codex", Prompt: "Reply ok"}, nil); err != nil {
		t.Fatal(err)
	}
	if p.Record.RoutePlan != plan || p.Record.FallbackPolicy != route.FallbackAgents || p.Record.Request.FallbackDefaulted {
		t.Fatalf("record plan=%p policy=%q defaulted=%v, want the admitted plan, %q, typed",
			p.Record.RoutePlan, p.Record.FallbackPolicy, p.Record.Request.FallbackDefaulted, route.FallbackAgents)
	}
	claim := &Record{ID: "claimed", FallbackPolicy: route.FallbackNone}
	p = &Prepared{Chain: Chain{Plan: plan}}
	if err := c.finishPrepare(context.Background(), p, RunOptions{Agent: "codex", Prompt: "Reply ok"}, claim); err != nil {
		t.Fatal(err)
	}
	if p.Record.RoutePlan != plan || p.Record.FallbackPolicy != route.FallbackNone {
		t.Fatalf("a claimed record got plan=%p policy=%q, want the admitted plan and its own policy none", p.Record.RoutePlan, p.Record.FallbackPolicy)
	}
}

func TestHopsLocalLast(t *testing.T) {
	local := &LocalHop{Workspace: Workspace{Profile: "local", Account: "magnitude·local"}}
	p := RoutePlan{Agent: "claude", Runners: []Runner{{RunnerCLI, "claude"}}, Pool: []Workspace{{Profile: "dev"}}, Local: local}
	for stage, want := range map[string]int{"shadow": 1, "last": 2} {
		setLocalStage(t, stage)
		if hops := p.Hops(); len(hops) != want || want == 2 && hops[1].Workspace.Profile != "local" {
			t.Errorf("stage %s: hops = %+v, want %d with local last", stage, hops, want)
		}
	}
}

// Given an overlay fence of qa, Then the plan's token scope admits qa and refuses prod.
func TestPlanRouteOverlayFence(t *testing.T) {
	autoFixture(t)
	ctx := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: "dev", route.OverlayVar: `{"fence":{"profiles":["qa"]}}`})
	p, err := PlanRoute(ctx, RunOptions{Agent: "codex", Prompt: "Reply ok", Provider: "prod"}, ParentRoute{Fence: route.Fence{Open: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx = withPlan(ctx, p)
	if _, err := TokenScope(ctx, "prod"); err == nil || !strings.Contains(err.Error(), "outside the confinement fence") {
		t.Errorf("TokenScope(prod) = %v, want the fence refusal", err)
	}
	if _, err := TokenScope(ctx, "qa"); err != nil {
		t.Errorf("TokenScope(qa) = %v, want it admitted", err)
	}
}

// Given an overlay maxDepth of 1, Then a grandchild dispatch is refused.
func TestOverlayMaxDepthStopsGrandchild(t *testing.T) {
	setConfigDir(t)
	one := 1
	fixtureRecord(t, Record{ID: "kid", Agent: "codex", Status: "running", Depth: 1, MaxDepth: &one})
	vals := proc.Values{route.OverlayVar: `{"maxDepth":1}`, proc.DispatchIDEnv: "kid", proc.DispatchDepthEnv: "1"}
	_, err := buildAskRecord(t.Context(), "codex", RunOptions{Agent: "codex", Values: vals}, nil, "", false)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "depth 2 exceeds cap 1") {
		t.Fatalf("grandchild err = %v, want the depth cap refusal", err)
	}
}

// Given a --retry plan that decided a closed empty fence, Then parentRoute keeps it closed.
func TestParentRouteKeepsClosedEmptyFence(t *testing.T) {
	ctx := WithRetried(context.Background(), Retried{ID: "r", Plan: &RoutePlan{Fenced: true}})
	if f := parentRoute(ctx).Fence; f.Open || len(f.Profiles) > 0 {
		t.Errorf("fence %+v, want closed and empty", f)
	}
	ctx = WithRetried(context.Background(), Retried{ID: "r", Plan: &RoutePlan{}})
	if !parentRoute(ctx).Fence.Open {
		t.Error("a plan that lacked a fence must read as open")
	}
}
