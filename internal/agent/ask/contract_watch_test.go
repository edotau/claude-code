package ask

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/platform/paths"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/contract"
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

// TestCancelledFleetRecoversSameContractOnce exercises one slot, recovery takeover and the retry limit.
func TestCancelledFleetRecoversSameContractOnce(t *testing.T) {
	lostLeaf(t, "owner", "leaf")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	settleHeld(ctx, &agent.Prepared{Record: agent.Record{ID: "leaf"}}, wire.Result{}, context.Canceled)
	assertWorkingHolders(t, "leaf", []string{"leaf"})
	// The watcher uses agents ask --retry <id> --recover, not a fresh spec.
	calls := 0
	send := func(_ context.Context, holder string) (string, error) {
		calls++
		return recoverCancelledLeaf(t, holder), nil
	}
	watchOnce(context.Background(), "owner", send, map[string]int{})
	k, err := contract.Load("leaf")
	p, _ := contract.Load("owner")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.Holders, []string{"leaf", "retry"}) || !slices.Equal(p.Children, []string{"leaf"}) || calls != 1 {
		t.Fatalf("not recovered in same slot: leaf %+v parent %+v calls %d", k, p, calls)
	}
	// A delayed cancellation from the old attempt cannot fail the new holder.
	settleHeld(ctx, &agent.Prepared{Record: agent.Record{ID: "leaf"}}, wire.Result{}, context.Canceled)
	assertWorkingHolders(t, "leaf", []string{"leaf", "retry"})
	settleHeld(ctx, &agent.Prepared{Record: agent.Record{ID: "retry"}}, wire.Result{}, context.Canceled)
	k, _ = contract.Load("leaf")
	if k.State() != contract.StateFailed || k.Failed != contract.ReasonCancelled {
		t.Fatalf("second cancel: %+v", k)
	}
	watchOnce(context.Background(), "owner", send, map[string]int{})
	if calls != 1 {
		t.Fatal("second cancelled attempt re-sent again")
	}
}

func recoverCancelledLeaf(t *testing.T, holder string) string {
	t.Helper()
	if holder != "leaf" {
		t.Fatalf("resend holder = %q", holder)
	}
	markResumed(t, holder, "retry")
	if got := heldID(agent.Record{ID: "retry"}); got != "leaf" {
		t.Fatalf("explicit recovery held %q, want leaf", got)
	}
	return "retry"
}

func assertWorkingHolders(t *testing.T, id string, holders []string) {
	t.Helper()
	k, err := contract.Load(id)
	if err != nil || k.State() != contract.StateWorking || !slices.Equal(k.Holders, holders) {
		t.Fatalf("contract %s: state %s holders %v err %v, want working %v", id, k.State(), k.Holders, err, holders)
	}
}

func TestParentCancelEndsRecoverableLeavesAndOldAbandonCannotFailRetry(t *testing.T) {
	lostLeaf(t, "owner", "leaf")
	if _, err := abandonContract("leaf", "leaf", contract.ReasonCancelled); err != nil {
		t.Fatal(err)
	}
	markResumed(t, "leaf", "retry")
	if got := heldID(agent.Record{ID: "retry"}); got != "leaf" {
		t.Fatal(got)
	}
	if _, err := abandonContract("leaf", "leaf", contract.ReasonOwnerDead); err != nil {
		t.Fatal(err)
	}
	assertWorkingHolders(t, "leaf", []string{"leaf", "retry"})
	if _, err := abandonContract("owner", "owner", contract.ReasonCancelled); err != nil {
		t.Fatal(err)
	}
	k, err := contract.Load("leaf")
	if err != nil || k.Failed != contract.ReasonCancelled {
		t.Fatalf("orphan: %+v %v", k, err)
	}
}

func TestCancelledFleetReplacementClosesOriginalContractAndOwner(t *testing.T) {
	lostLeaf(t, "owner", "leaf")
	if _, err := abandonContract("leaf", "leaf", contract.ReasonCancelled); err != nil {
		t.Fatal(err)
	}
	recoverCancelledLeaf(t, "leaf")
	zero := 0
	ret := contractReturn{End: endFinished, Gate: &worktree.GateRun{Exit: &zero},
		Verify:   &wire.VerifyResult{Command: "go test ./internal/contract -run TestTransitions", Exit: 0},
		Handback: &wire.Handback{Status: "DONE", Summary: "done"}, Tests: contract.TestsExecuted,
		Method: contract.MethodSolo, Report: completeReport(t, "leaf.md")}
	k, err := settleContract("leaf", "retry", ret)
	if err != nil || k.State() != contract.StateClosed {
		t.Fatalf("replacement did not close original contract: %+v %v", k, err)
	}
	ret.Children, ret.Method, ret.Report = []contract.Contract{k}, contract.MethodFleet, completeReport(t, "owner.md")
	p, err := settleContract("owner", "owner", ret)
	if err != nil || p.State() != contract.StateClosed || !slices.Equal(p.Children, []string{"leaf"}) {
		t.Fatalf("owner did not close with one recovered leaf: %+v %v", p, err)
	}
}

func TestFailedOwnerSettlesPreservedDeadLeafWithItsReason(t *testing.T) {
	for _, why := range []contract.Reason{contract.ReasonDeadline, contract.ReasonOwnerDead, contract.ReasonTerm} {
		t.Run(string(why), func(t *testing.T) {
			lostLeaf(t, "owner", "leaf")
			if _, err := abandonContract("leaf", "leaf", contract.ReasonCancelled); err != nil {
				t.Fatal(err)
			}
			assertWorkingHolders(t, "leaf", []string{"leaf"})
			if _, err := abandonContract("owner", "owner", why); err != nil {
				t.Fatal(err)
			}
			k, err := contract.Load("leaf")
			if err != nil || k.Failed != why {
				t.Fatalf("dead leaf orphaned or misattributed: %+v %v", k, err)
			}
		})
	}
}

func TestWatchDoesNotResendAfterOwnerSettles(t *testing.T) {
	lostLeaf(t, "owner", "leaf")
	if _, err := contract.Update("owner", func(c contract.Contract) (contract.Contract, error) { return c.Fail(contract.ReasonCancelled) }); err != nil {
		t.Fatal(err)
	}
	calls := 0
	watchOnce(context.Background(), "owner", func(context.Context, string) (string, error) { calls++; return "retry", nil }, map[string]int{})
	if calls != 0 {
		t.Fatal("terminal owner watcher re-sent a leaf")
	}
}

func TestTerminalContractRecoveryRefusesBeforeModelRun(t *testing.T) {
	lostLeaf(t, "owner", "leaf")
	markResumed(t, "leaf", "retry")
	if _, err := abandonContract("owner", "owner", contract.ReasonCancelled); err != nil {
		t.Fatal(err)
	}
	p := &agent.Prepared{Record: agent.Record{ID: "retry", Agent: "claude", Status: "running", Time: time.Now()}}
	if err := p.Record.Save(); err != nil {
		t.Fatal(err)
	}
	// A nil client would panic if the rejected recovery reached model execution.
	if code := runSettled(context.Background(), nil, p, agent.RunOptions{}, nil, false, "text", "", io.Discard); code != 1 {
		t.Fatalf("terminal recovery exit = %d", code)
	}
	rec, err := agent.LoadRecord("retry")
	if err != nil || rec.Status != wire.StatusError || !strings.Contains(rec.Error, "contract leaf: state failed") {
		t.Fatalf("refusal was not concluded: %+v %v", rec, err)
	}
}

// TestClaimRecoverGate: --recover of a cancelled dispatch is refused at launch; a plain --retry is not gated.
func TestClaimRecoverGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	rec.Status, rec.FailKind = "error", agent.FailCancelled
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), recoverKey{}, &recoverClaim{from: rec.ID})
	if err := claimRecover(ctx); !errors.Is(err, agent.ErrNotResent) {
		t.Fatalf("recover of a cancelled dispatch: %v, want not re-sent", err)
	}
	if err := claimRecover(context.Background()); err != nil {
		t.Fatalf("plain retry was gated: %v", err)
	}
}

// TestRefusedFleetStartLeavesNoRunningRecord: a fleet start refused for its parent settles the claimed record as error.
func TestRefusedFleetStartLeavesNoRunningRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	o := &contractOpen{Scope: contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		Owner: contract.Party{Role: contract.RoleFleet, Agent: "pi"}, Limits: contract.Limits{Depth: contract.MaxDepth},
		Parent: "no-such-owner"}
	if err := startAsk(withContract(context.Background(), o), nil, &rec); err == nil {
		t.Fatal("fleet start with a missing parent was not refused")
	}
	got, err := agent.LoadRecord(rec.ID)
	if err != nil || got.Status != wire.StatusError {
		t.Fatalf("record after refusal: %v, status %q error %q; want error", err, got.Status, got.Error)
	}
}
