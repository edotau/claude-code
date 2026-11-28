package agent

import (
	"cmp"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"reflect"
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

func TestPlanRunnerSelectionAndHopOrder(t *testing.T) {
	_, ctx := plannedRunnerFixture(t)
	for _, tc := range []struct {
		name, runner, fallback string
		carry                  bool
		want                   []RunnerKind
	}{
		{name: "Databricks Pi first", fallback: "all", want: []RunnerKind{RunnerPi, RunnerPi, RunnerCLI}},
		{name: "runner cli", runner: "cli", fallback: "all", want: []RunnerKind{RunnerCLI}},
		{name: "runner pi", runner: "pi", fallback: "all", want: []RunnerKind{RunnerPi, RunnerPi, RunnerCLI}},
		{name: "none retains own CLI", fallback: "none", want: []RunnerKind{RunnerPi, RunnerCLI}},
		{name: "pin retains sibling runners", fallback: "pin", want: []RunnerKind{RunnerPi, RunnerPi, RunnerCLI}},
		{name: "carry keeps own CLI", runner: "pi", fallback: "all", carry: true, want: []RunnerKind{RunnerCLI}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := RunOptions{Agent: "claude", Model: "sonnet", Lead: "workspace-a", Runner: tc.runner, Fallback: tc.fallback, Carry: tc.carry}
			p, err := PlanRoute(ctx, o, ParentRoute{Fence: route.Fence{Open: true}})
			if err != nil {
				t.Fatal(err)
			}
			var got []RunnerKind
			for _, h := range p.Hops() {
				got = append(got, h.Runner.Kind)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("hops = %+v, want %v", p.Hops(), tc.want)
			}
			q := RouteReq{Agent: o.Agent, Model: o.Model, Fallback: o.Fallback, Opts: o}
			lead := Dispatch{Agent: &fakeRunner{name: o.Agent}, Model: o.Model, Account: "databricks·workspace-a"}
			ch := planTestChain(withPlan(ctx, p), q, Chain{Dispatches: []Dispatch{lead}})
			admitted := admittedPlan(p, ch)
			if !slices.Equal(admitted.Hops(), p.Hops()) {
				t.Fatalf("admitted hops = %+v, planned %+v (notes %+v)", admitted.Hops(), p.Hops(), ch.Preflight)
			}
		})
	}
}

func TestAdmittedPlanMatchesSingleResolvedChain(t *testing.T) {
	for _, seq := range [][]string{
		{"pi@databricks·dev", "pi@databricks·qa", "codex@databricks·dev", "codex@openai·chatgpt", "claude@databricks·dev"},
		{"codex@databricks·qa", "codex@databricks·dev", "claude@databricks·qa"},
		{"pi@databricks·dev", "claude@databricks·dev"},
		{"pi@databricks·dev"},
		{"pi@databricks·qa", "codex@openai·chatgpt"},
	} {
		ch := planChainOf(seq...)
		p := admittedPlan(RoutePlan{Agent: "pi", Runners: []Runner{{Kind: RunnerPi, Agent: "pi"}, {Kind: RunnerCLI, Agent: "codex"}, {Kind: RunnerCLI, Agent: "claude"}}}, ch)
		if len(p.Admitted) != len(ch.Dispatches) {
			t.Fatalf("%v: admitted %d hops, want %d", seq, len(p.Admitted), len(ch.Dispatches))
		}
		for i, h := range p.Admitted {
			if h.Workspace.Account != ch.Dispatches[i].Account {
				t.Errorf("%v: hop %d admitted on %q, chain ran %q", seq, i+1, h.Workspace.Account, ch.Dispatches[i].Account)
			}
		}
	}
}

func TestPiRunnerWithoutPlan(t *testing.T) {
	ch := planChainOf("codex@databricks·dev")
	got := planTestChain(context.Background(), RouteReq{Agent: "codex"}, ch)
	if len(got.Dispatches) != 1 || got.Dispatches[0].Agent.Name() != "codex" {
		t.Fatalf("no runner decision widened chain: %+v", got)
	}
}

func TestAdmittedPlanRetainsCandidatesAndPiRefusal(t *testing.T) {
	p := RoutePlan{Agent: "codex", Runners: []Runner{{Kind: RunnerPi, Agent: "codex"}, {Kind: RunnerCLI, Agent: "codex"}},
		Pool: []Workspace{{Profile: "dev", Account: "databricks·dev"}, {Profile: "qa", Account: "databricks·qa"}}}
	ch := planChainOf("codex@databricks·dev", "codex@github·a-login", "claude@databricks·dev")
	ch = ch.noted("codex: pi runner skipped: pi is not installed")
	got := admittedPlan(p, ch)
	if len(got.Pool) != 2 || got.Pool[1].Profile != "qa" || got.Barred != "pi is not installed" {
		t.Fatalf("candidate pool or validation refusal lost: %+v", got)
	}
	if len(got.Runners) != 1 || got.Runners[0].Kind != RunnerCLI || got.Admitted[1].Workspace.Profile != "" {
		t.Fatalf("Pi retained or billing login became a profile: %+v", got)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("same-account different-agent tail lost: %+v", got.Agents)
	}
}

func TestPlanOptionsPreservesFallback(t *testing.T) {
	autoFixture(t)
	defs, err := ParseAgents(`{"worker":{"agent":"claude","prompt":"p","model":"sonnet"}}`)
	if err != nil {
		t.Fatal(err)
	}
	cl := &Client{}
	for _, req := range []RunOptions{
		{Agent: "claude", Model: "sonnet", Fallback: "all"},
		{Agent: "worker", Agents: defs, Fallback: "all"},
	} {
		got, err := cl.planOptions(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Fallback != "all" {
			t.Fatalf("fallback or typed provenance changed: input %+v, output %+v", req, got)
		}
	}
}

func TestPlanResolvePreviewRunnerOrder(t *testing.T) {
	_, ctx := plannedRunnerFixture(t)
	var cliPick router.PickResult
	for _, runner := range []string{"cli", "pi"} {
		q := RouteReq{Agent: "claude", Model: "sonnet", Lead: "workspace-a", Fallback: "all", Preview: true,
			Opts: RunOptions{Runner: runner}}
		ch, pick, err := PreviewRoute(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(ch.Dispatches) == 0 {
			t.Fatalf("empty %s preview: %+v", runner, ch)
		}
		if runner == "cli" {
			cliPick = pick
			if ch.Dispatches[0].Agent.Name() != q.Agent {
				t.Fatalf("CLI preview = %+v", ch)
			}
			continue
		}
		if ch.runners() != 2 || ch.OwnHop() != 2 || ch.Dispatches[2].Agent.Name() != q.Agent {
			t.Fatalf("Pi preview = %+v", ch)
		}
		if !reflect.DeepEqual(pick.Candidates, cliPick.Candidates) {
			t.Fatalf("runner hops changed preview candidates: %+v, CLI %+v", pick.Candidates, cliPick.Candidates)
		}
		if ch.Preflight == nil || ch.Preflight.Chosen != hopTarget(ch.Dispatches[0]) {
			t.Fatalf("preflight did not choose Pi: %+v", ch.Preflight)
		}
	}
}

func TestPlanResolveExplicitRunnerOrder(t *testing.T) {
	_, ctx := plannedRunnerFixture(t)
	for _, runner := range []string{"pi", "cli"} {
		o := RunOptions{Agent: "claude", Model: "sonnet", Provider: "workspace-a", Fallback: "all", Runner: runner}
		p, err := PlanRoute(ctx, o, ParentRoute{Fence: route.Fence{Open: true}})
		if err != nil {
			t.Fatal(err)
		}
		p.Pool = append(p.Pool, Workspace{Profile: "workspace-b", Account: "databricks·workspace-b"})
		engine, err := Lookup(o.Agent)
		if err != nil {
			t.Fatal(err)
		}
		q := RouteReq{Agent: o.Agent, Model: o.Model, Profile: o.Provider, Pinned: true, Fallback: o.Fallback, Engine: engine, Opts: o}
		ch, _, err := Resolve(withPlan(ctx, p), q)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if runner == "pi" {
			want = 2
		}
		if len(ch.Dispatches) != want || ch.Dispatches[want-1].Agent.Name() != o.Agent || !ch.Dispatches[want-1].unroutedDispatch {
			t.Fatalf("explicit %s chain = %+v", runner, ch)
		}
		if runner == "pi" && ch.Dispatches[0].runFor != o.Agent {
			t.Fatalf("no Pi runner: %+v", ch)
		}
	}
}

func TestPlanChainRecordsRunnerFallbacks(t *testing.T) {
	_, ctx := plannedRunnerFixture(t)
	o := RunOptions{Agent: "claude", Model: "sonnet", Lead: "workspace-a", Fallback: "all", Runner: "pi"}
	engine, err := Lookup(o.Agent)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := (&Client{}).planChain(ctx, engine, o)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Plan == nil || ch.runners() != 2 || ch.OwnHop() != 2 || len(ch.Plan.Admitted) != len(ch.Dispatches) {
		t.Fatalf("planned chain = %+v", ch)
	}
	for i, d := range ch.Dispatches {
		h := ch.Plan.Hops()[i]
		if h.Workspace.Account != d.Account || h.Runner.Agent != d.agentName() {
			t.Fatalf("hop %d = %+v, dispatch %+v", i, h, d)
		}
	}
}

func TestPlanAdmissionRejectsCLIBeforeRunner(t *testing.T) {
	reg, ctx := plannedRunnerFixture(t)
	p := RoutePlan{Agent: "claude", Runners: []Runner{{Kind: RunnerPi, Agent: "claude"}}}
	ctx = context.WithValue(withPlan(ctx, p), routeRequestKey{}, RouteReq{Agent: p.Agent})
	assembly := newPlannedChain(ctx, reg)
	assembly.checked[p.Agent] = &Finding{Check: "cli-version", Detail: "required CLI version"}
	lead := Dispatch{Agent: &fakeRunner{name: p.Agent}, Account: "databricks·workspace-a", Model: "sonnet"}
	c := assembly.admit(Chain{}, lead, &Preflight{})
	if len(c.Dispatches) != 0 {
		t.Fatalf("refused CLI acquired a Pi prefix: %+v", c)
	}
	tail := Dispatch{Agent: &fakeRunner{name: "codex"}, Account: "openai·login"}
	c = assembly.admit(c, tail, &Preflight{})
	if len(c.Dispatches) != 1 || c.Dispatches[0].Agent != tail.Agent {
		t.Fatalf("fallback = %+v", c)
	}
}

func TestRoutePlanFilesZeroExecImports(t *testing.T) {
	files := []string{
		"routeplan.go", "routeplan_chain.go", "route.go", "modelroute.go",
		"rotation.go", "pirunner.go", "localhop.go", "preflight.go",
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			if name == "routeplan_chain.go" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "os/exec" {
				t.Errorf("%s imports os/exec; pure route-planning files must not shell out", name)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no route-planning files parsed")
	}
}
