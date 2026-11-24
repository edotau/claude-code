package ask

import (
	"context"
	"errors"
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
