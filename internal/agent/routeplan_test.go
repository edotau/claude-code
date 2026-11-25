package agent

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
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
	{name: "runner own", o: RunOptions{Agent: "codex", Prompt: "Reply ok", Runner: "own"}, check: checkRunnerOwn},
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
			p, err := PlanRoute(ctx, r.o, ParentRoute{Fence: route.Fence{Open: true}})
			if err != nil {
				t.Fatal(err)
			}
			r.check(t, p)
		})
	}
}

func checkAutoPick(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerPi, RunnerOwn)
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
	wantRunners(t, p, RunnerOwn)
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
		if !p.Fence.Allows(h.Workspace.Profile) {
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

func checkRunnerOwn(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerOwn)
	if !strings.Contains(p.Barred, "--runner own") {
		t.Errorf("barred = %q, want it to name --runner own", p.Barred)
	}
}

func checkTypedNone(t *testing.T, p RoutePlan) {
	wantRunners(t, p, RunnerPi, RunnerOwn)
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
	wantRunners(t, p, RunnerOwn)
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
	if d := Divergence(p, planChainOf("codex@databricks·prod")); len(d) == 0 || !strings.Contains(d[0], "chain codex@databricks·prod") {
		t.Errorf("divergence = %q, want the prod pick named", d)
	}
}

// checkStandardAlias: baseline noted codex's opus alias while the hop ran the standard model.
func checkStandardAlias(t *testing.T, p RoutePlan) {
	if p.Tier.Asked != models.Sonnet || p.Tier.Alias != models.Sonnet || strings.Contains(p.Tier.Note, models.Opus) {
		t.Errorf("tier = %+v, want sonnet with no opus alias note", p.Tier)
	}
	if d := Divergence(p, planChainOf("pi@databricks·dev", "codex@databricks·dev")); d != nil {
		t.Errorf("divergence against the dry-run chain = %q, want none", d)
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
