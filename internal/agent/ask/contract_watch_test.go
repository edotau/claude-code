package ask

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"

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
	tries := map[string]int{}
	watchOnce(context.Background(), "d-1", send, tries)
	if k, _ := contract.Load("d-2"); len(k.Holders) != 2 || k.State() != contract.StateWorking {
		t.Fatalf("after first loss: holders %v state %s, want re-assigned and working", k.Holders, k.State())
	}
	watchOnce(context.Background(), "d-1", send, tries)
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

// lostLeaf opens owner and one leaf whose dispatch record is already dead.
func lostLeaf(t *testing.T, owner, leaf string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := leavesGrace
	leavesGrace = 0
	t.Cleanup(func() { leavesGrace = old })
	if err := startFleet(t, "", owner, contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, owner, leaf, contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	dead := agent.Record{ID: leaf, Status: "failed", Time: time.Now().Add(-time.Minute)}
	if err := dead.Save(); err != nil {
		t.Fatal(err)
	}
}

// TestWatchOnceRetriesTransientResend: a re-send that errs twice is retried on the next polls.
func TestWatchOnceRetriesTransientResend(t *testing.T) {
	lostLeaf(t, "d-1", "d-2")
	calls, okAfter := 0, 2
	send := func(context.Context, string) (string, error) {
		calls++
		if calls <= okAfter {
			return "", errors.New("transient")
		}
		return "d-4", nil
	}
	tries := map[string]int{}
	for range 2 {
		watchOnce(context.Background(), "d-1", send, tries)
	}
	if k, _ := contract.Load("d-2"); k.State() != contract.StateWorking || len(k.Holders) != 1 {
		t.Fatalf("after two transient errors: %s holders %v, want working and unchanged", k.State(), k.Holders)
	}
	watchOnce(context.Background(), "d-1", send, tries)
	if k, _ := contract.Load("d-2"); len(k.Holders) != 2 || tries["d-2"] != 0 {
		t.Fatalf("after success: holders %v tries %v, want re-assigned", k.Holders, tries)
	}
}

// TestWatchOnceFailsAfterRepeatedResendErrors: the third consecutive re-send error fails the leaf owner-dead.
func TestWatchOnceFailsAfterRepeatedResendErrors(t *testing.T) {
	lostLeaf(t, "e-1", "e-2")
	bad := func(context.Context, string) (string, error) { return "", errors.New("transient") }
	tries := map[string]int{}
	for range resendTries {
		watchOnce(context.Background(), "e-1", bad, tries)
	}
	if k, _ := contract.Load("e-2"); k.State() != contract.StateFailed || k.Failed != contract.ReasonOwnerDead {
		t.Fatalf("after %d errors: %s %q, want failed owner-dead", resendTries, k.State(), k.Failed)
	}
}

// TestSettleLeavesWaitsForWorking: the wait ends when the leaf closes, and names a leaf still working at the cap.
func TestSettleLeavesWaitsForWorking(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldPoll, oldCap := leavesSettlePoll, leavesSettle
	leavesSettlePoll = 10 * time.Millisecond
	t.Cleanup(func() { leavesSettlePoll, leavesSettle = oldPoll, oldCap })
	if err := startFleet(t, "", "d-1", contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "d-1", "d-2", contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = contract.Update("d-2", func(k contract.Contract) (contract.Contract, error) {
			return k.Fail(contract.ReasonOwnerDead)
		})
	}()
	if open := settleLeaves(context.Background(), []string{"d-2"}); len(open) != 0 {
		t.Fatalf("open = %v, want none once the leaf closed", open)
	}
	if err := startFleet(t, "", "e-1", contract.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := startFleet(t, "e-1", "e-2", contract.RoleFleet); err != nil {
		t.Fatal(err)
	}
	leavesSettle = 50 * time.Millisecond
	if open := settleLeaves(context.Background(), []string{"e-2"}); len(open) != 1 || open[0] != "e-2" {
		t.Fatalf("open = %v, want e-2 still working", open)
	}
}
