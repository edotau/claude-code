package ask

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
)

// Two batches started in the same millisecond still get distinct batch ids.
func TestNewIDDistinctAtOneInstant(t *testing.T) {
	old := batchNow
	batchNow = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { batchNow = old })
	a, b := NewID(), NewID()
	if a == b || !strings.HasPrefix(a, "batch-20261029T120000.000") {
		t.Errorf("ids %q and %q: want distinct ids that keep the batch-<stamp> prefix", a, b)
	}
}

// plan_task is validated per line: a bad one fails only its task, with the line number; a heading with a space passes.
func TestParseTaskPlanTask(t *testing.T) {
	in := `{"id":"a","prompt":"p","plan_task":"Phase 1.1"}` + "\n" + `{"id":"b","prompt":"p","plan_task":"bad/id"}` + "\n"
	lines, err := ReadTaskLines(strings.NewReader(in))
	if err != nil || len(lines) != 2 {
		t.Fatalf("ReadTaskLines = %d lines, %v", len(lines), err)
	}
	seen := map[string]bool{}
	good, err := ParseTask(lines[0], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err != nil || good.Plan != "Phase 1.1" {
		t.Fatalf("Phase 1.1: plan %q, err %v", good.Plan, err)
	}
	bad, err := ParseTask(lines[1], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "plan_task") {
		t.Fatalf("bad/id error = %v, want line 2 plan_task error", err)
	}
	if bad.ID != "b" || bad.Plan != "" {
		t.Errorf("rejected task = id %q plan %q, want id kept and no plan", bad.ID, bad.Plan)
	}
}

// runRetryBatch runs tasks with one job and returns the ok flag and the decoded envelopes by task id.
func runRetryBatch(t *testing.T, o PoolOptions, tasks ...Task) (bool, map[string][]wire.Envelope) {
	t.Helper()
	var buf bytes.Buffer
	o.Out, o.ID, o.Jobs = &buf, "batch-r", 1
	ok := RunTasks(t.Context(), tasks, o)
	byID := map[string][]wire.Envelope{}
	for _, env := range decodePoolEnvelopes(t, &buf) {
		byID[env.TaskID] = append(byID[env.TaskID], env)
	}
	return ok, byID
}

// A real failure is retried once under the same task id: one envelope, the retry's, both attempts in hops.
func TestRetryAfterFailureEmitsOneEnvelope(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	_, got := runRetryBatch(t, PoolOptions{}, vendorTask(t, "a", "Phase 1.1", "FAILME", "ok", &prep, &again))
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusOK || len(got["a"][0].Hops) < 2 {
		t.Fatalf("envelopes for a = %+v, want one ok envelope with >= 2 hops", got["a"])
	}
	if prep.Load() != 1 || again.Load() != 1 {
		t.Errorf("prepare/retry calls = %d/%d, want 1/1", prep.Load(), again.Load())
	}
	var recs int
	for _, r := range agent.RecentRecords(10) {
		recs++
		if r.TaskID != "a" || r.BatchID != "batch-r" || r.PlanTask != "Phase 1.1" {
			t.Errorf("record %s = task %q batch %q plan %q", r.ID, r.TaskID, r.BatchID, r.PlanTask)
		}
	}
	if recs != 2 {
		t.Errorf("%d dispatch records, want 2 (first attempt and retry)", recs)
	}
}

// A retry that fails too is the task's one envelope, and the task never runs a third time.
func TestRetryAlsoFailsEmitsRetryError(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	ok, got := runRetryBatch(t, PoolOptions{}, vendorTask(t, "a", "", "FAILME", "FAILME", &prep, &again))
	if ok || len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError || len(got["a"][0].Hops) != 2 {
		t.Fatalf("ok=%v envelopes for a = %+v, want one error envelope with 2 hops", ok, got["a"])
	}
	if prep.Load() != 1 || again.Load() != 1 {
		t.Errorf("prepare/retry calls = %d/%d, want 1/1", prep.Load(), again.Load())
	}
}

// With one slot a retried task keeps its slot: later tasks still run and the batch ends.
func TestRetryJobsOneKeepsSlotAccounting(t *testing.T) {
	vendorHost(t)
	var n, again atomic.Int32
	done := make(chan map[string][]wire.Envelope, 1)
	go func() {
		_, got := runRetryBatch(t, PoolOptions{},
			vendorTask(t, "a", "", "FAILME", "ok", &n, &again),
			vendorTask(t, "b", "", "ok", "", &n, &n),
			vendorTask(t, "c", "", "ok", "", &n, &n))
		done <- got
	}()
	select {
	case got := <-done:
		for _, id := range []string{"a", "b", "c"} {
			if len(got[id]) != 1 || got[id][0].Status != wire.StatusOK {
				t.Errorf("task %s envelopes = %+v, want one ok", id, got[id])
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("batch deadlocked: a retried task leaked its slot")
	}
}

// No retry without a dispatch (no hops), after a prepare error, after cancellation, or under FailFast.
func TestRetrySkippedWhenNotRealFailure(t *testing.T) {
	vendorHost(t)
	var retried atomic.Int32
	retry := func(context.Context) (*agent.Prepared, error) { retried.Add(1); return nil, errors.New("retry ran") }
	noHops := func(context.Context) (*agent.Prepared, error) {
		return &agent.Prepared{Record: agent.NewRecord("opencode", agent.RunOptions{}, time.Now())}, nil
	}
	badPrep := func(context.Context) (*agent.Prepared, error) { return nil, errors.New("prepare failed") }
	runRetryBatch(t, PoolOptions{},
		Task{ID: "h", Agent: "opencode", Prepare: noHops, Retry: retry},
		Task{ID: "p", Agent: "opencode", Prepare: badPrep, Retry: retry})
	if retried.Load() != 0 {
		t.Errorf("Retry ran %d times for a no-hop failure and a prepare error, want 0", retried.Load())
	}
	var prep, again atomic.Int32
	_, got := runRetryBatch(t, PoolOptions{FailFast: true},
		vendorTask(t, "a", "", "FAILME", "ok", &prep, &again), vendorTask(t, "b", "", "ok", "", &prep, &prep))
	if again.Load() != 0 || len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError {
		t.Errorf("FailFast: retry calls %d, a envelopes %+v, want 0 retries and a's own error", again.Load(), got["a"])
	}
}

// A batch cancelled while the first attempt runs does not start the retry.
func TestRetrySkippedWhenCancelled(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	task := vendorTask(t, "a", "", "FAILME", "ok", &prep, &again)
	inner := task.Prepare
	task.Prepare = func(c context.Context) (*agent.Prepared, error) {
		p, err := inner(c)
		cancel()
		return p, err
	}
	var buf bytes.Buffer
	RunTasks(ctx, []Task{task}, PoolOptions{ID: "batch-c", Jobs: 1, Out: &buf})
	if again.Load() != 0 || len(decodePoolEnvelopes(t, &buf)) != 1 {
		t.Errorf("retry calls = %d, want 0 and one envelope", again.Load())
	}
}
