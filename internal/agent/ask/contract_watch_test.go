package ask

import (
	"context"
	"errors"
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
)

func startFleet(t *testing.T, parent, id string, role contract.Role) error {
	t.Helper()
	lim := contract.Limits{Depth: 1, Leaves: 1}
	if role == contract.RoleFleet {
		lim = contract.Limits{Depth: contract.MaxDepth}
	}
	o := &contractOpen{Scope: contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		Owner: contract.Party{Role: role, Agent: "pi"}, Limits: lim, Parent: parent,
		Facts: contract.Facts{Fleet: role == contract.RoleOwner}}
	rec := agent.Record{ID: id, Status: wire.StatusOK, Time: time.Now()}
	return startAsk(withContract(context.Background(), o), nil, &rec)
}

// TestFleetJoinRefusesPastLeafBudget: a fleet start joins its owner's children; the one past the budget is refused.
func TestFleetJoinRefusesPastLeafBudget(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := startFleet(t, "", "d-1", contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "d-1", "d-2", contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "d-1", "d-3", contract.RoleFleet); !errors.Is(err, contract.ErrLimit) {
		t.Fatalf("third leaf err = %v, want a limit refusal", err)
	}
	if c, _ := contract.Load("d-1"); len(c.Children) != 1 {
		t.Fatalf("children = %v, want only d-2", c.Children)
	}
}

// TestWatchOnceResendsOnceThenFails: a lost leaf is re-assigned to its re-send; a second loss fails it owner-dead.
func TestWatchOnceResendsOnceThenFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := leavesGrace
	leavesGrace = 0
	t.Cleanup(func() { leavesGrace = old })
	if err := startFleet(t, "", "d-1", contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "d-1", "d-2", contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	dead := agent.Record{ID: "d-2", Status: "failed", Time: time.Now().Add(-time.Minute)}
	if err := dead.Save(); err != nil {
		t.Fatal(err)
	}
	next := agent.Record{ID: "d-4", Status: "failed", Time: time.Now().Add(-time.Minute)}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	send := func(context.Context, string) (string, error) { return "d-4", nil }
	watchOnce(context.Background(), "d-1", send)
	if k, _ := contract.Load("d-2"); len(k.Holders) != 2 || k.State() != contract.StateWorking {
		t.Fatalf("after first loss: holders %v state %s, want re-assigned and working", k.Holders, k.State())
	}
	watchOnce(context.Background(), "d-1", send)
	if k, _ := contract.Load("d-2"); k.State() != contract.StateFailed || k.Failed != contract.ReasonOwnerDead {
		t.Fatalf("after second loss: %s %q, want failed owner-dead", k.State(), k.Failed)
	}
}

// TestHeldIDTakesOverResentContract: a re-sent dispatch named by the resend env takes the leaf's contract itself.
func TestHeldIDTakesOverResentContract(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := startFleet(t, "", "d-1", contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "d-1", "d-2", contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	if got := heldID(agent.Record{ID: "d-5"}); got != "" {
		t.Fatalf("without a marker heldID = %q, want none", got)
	}
	markResumed(t, "d-2", "d-5")
	markResumed(t, "d-1", "d-7")
	if got := heldID(agent.Record{ID: "d-5"}); got != "d-2" {
		t.Fatalf("heldID = %q, want d-2", got)
	}
	if got := heldID(agent.Record{ID: "d-7"}); got != "d-1" { // a retried owner takes its contract over too
		t.Fatalf("retried owner heldID = %q, want d-1", got)
	}
	if k, _ := contract.Load("d-2"); len(k.Holders) != 2 || k.Holders[1] != "d-5" {
		t.Fatalf("holders = %v, want d-2 then d-5", k.Holders)
	}
}

func markResumed(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(agent.WatchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent.WatchMarker("resumed", from), []byte(to+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReportOfAcceptsSpecReportPath: a filed file at the spec's report path passes without `memory report --job`.
func TestReportOfAcceptsSpecReportPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := scratchFile(t, paths.MemorySubDir("reports"), "r.md")
	c := contract.Contract{Scope: contract.Scope{Report: p}}
	if got := reportOf(agent.Record{ID: "d-1"}, c); got != p {
		t.Fatalf("reportOf = %q, want %q", got, p)
	}
	if got := reportOf(agent.Record{ID: "d-1"}, contract.Contract{}); got != "" {
		t.Fatalf("no report: %q, want empty", got)
	}
}
