package agent

import (
	"context"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
	"reflect"
	"testing"
)

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
