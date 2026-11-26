package agent

import (
	"context"
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
	got := withPiRunner(context.Background(), RouteReq{Agent: "codex"}, ch)
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
