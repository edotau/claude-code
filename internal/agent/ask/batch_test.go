package ask

import (
	"strings"
	"testing"
	"time"

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
