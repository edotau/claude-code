package agent

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/router"
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
		p := admittedPlan(RoutePlan{Agent: "pi", Runners: []Runner{{Kind: RunnerPi, Agent: "pi"}, {Kind: RunnerOwn, Agent: "codex"}, {Kind: RunnerOwn, Agent: "claude"}}}, ch)
		if got := Divergence(p, ch); len(got) != 0 {
			t.Errorf("%v: divergence %v", seq, got)
		}
	}
}

func TestPiRunnerWithoutPlan(t *testing.T) {
	ch := planChainOf("codex@databricks·dev")
	got := withPiRunner(context.Background(), RouteReq{Agent: "codex"}, ch)
	if len(got.Dispatches) != 1 || got.Dispatches[0].Agent.Name() != "codex" {
		t.Fatalf("no runner decision widened chain: %+v", got)
	}
}

func TestAdmittedPlanRetainsCandidatesAndPiRefusal(t *testing.T) {
	p := RoutePlan{Agent: "codex", Runners: []Runner{{Kind: RunnerPi, Agent: "codex"}, {Kind: RunnerOwn, Agent: "codex"}},
		Pool: []Workspace{{Profile: "dev", Account: "databricks·dev"}, {Profile: "qa", Account: "databricks·qa"}}}
	ch := planChainOf("codex@databricks·dev", "codex@github·a-login", "claude@databricks·dev")
	ch = ch.noted("codex: pi runner skipped: pi is not installed")
	got := admittedPlan(p, ch)
	if len(got.Pool) != 2 || got.Pool[1].Profile != "qa" || got.Barred != "pi is not installed" {
		t.Fatalf("candidate pool or validation refusal lost: %+v", got)
	}
	if len(got.Runners) != 1 || got.Runners[0].Kind != RunnerOwn || got.Admitted[1].Workspace.Profile != "" {
		t.Fatalf("Pi retained or billing login became a profile: %+v", got)
	}
	if d := Divergence(p, ch); len(d) == 0 {
		t.Fatal("independent candidate plan must still expose validation differences")
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

func TestDivergenceDetectsAdmissionViolatingIntent(t *testing.T) {
	p := RoutePlan{Agent: "codex", Fallback: router.Policy{Kind: router.PolicySameWorkspace},
		Runners: []Runner{{Kind: RunnerOwn, Agent: "codex"}}, Pool: []Workspace{{Profile: "dev", Account: "databricks·dev"}}}
	ch := planChainOf("pi@databricks·qa", "codex@databricks·dev")
	p.Constraints = planConstraints(p, ch) // the real planChain captures violations before admission
	got := admittedPlan(p, ch)
	if d := Divergence(got, ch); len(d) != 2 {
		t.Fatalf("admission concealed the independent runner/workspace violations: %v", d)
	}
	ch.Dispatches = ch.Dispatches[1:]
	p.Constraints = planConstraints(p, ch)
	got = admittedPlan(p, ch)
	if d := Divergence(got, ch); len(d) != 0 {
		t.Fatalf("valid same-workspace admission diverged: %v", d)
	}
	ch.Dispatches[0].Account = "databricks·qa"
	if d := Divergence(got, ch); len(d) == 0 {
		t.Fatal("downstream chain mutation went undetected")
	}
}

func TestAdmissionKeepsPrevalidationConstraints(t *testing.T) {
	p := RoutePlan{Agent: "codex", Constraints: []string{"pi runner was not authorized"}}
	ch := planChainOf("codex@databricks·dev")
	got := admittedPlan(p, ch)
	if d := Divergence(got, ch); len(d) == 0 || d[0] != p.Constraints[0] {
		t.Fatalf("admission hid independent constraints: %v", d)
	}
}
