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
	a, b := newID(), newID()
	if a == b || !strings.HasPrefix(a, "batch-20261029T120000.000") {
		t.Errorf("ids %q and %q: want distinct ids that keep the batch-<stamp> prefix", a, b)
	}
}

// plan_task is validated per line: a bad one fails only its task, with the line number; a heading with a space passes.
func TestParseTaskPlanTask(t *testing.T) {
	in := `{"id":"a","prompt":"p","plan_task":"Phase 1.1"}` + "\n" + `{"id":"b","prompt":"p","plan_task":"bad/id"}` + "\n"
	lines, err := readTaskLines(strings.NewReader(in))
	if err != nil || len(lines) != 2 {
		t.Fatalf("readTaskLines = %d lines, %v", len(lines), err)
	}
	seen := map[string]bool{}
	good, err := parseTask(lines[0], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err != nil || good.Plan != "Phase 1.1" {
		t.Fatalf("Phase 1.1: plan %q, err %v", good.Plan, err)
	}
	bad, err := parseTask(lines[1], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "plan_task") {
		t.Fatalf("bad/id error = %v, want line 2 plan_task error", err)
	}
	if bad.ID != "b" || bad.Plan != "" {
		t.Errorf("rejected task = id %q plan %q, want id kept and no plan", bad.ID, bad.Plan)
	}
}

// A line with trailing junk after its object is that line's error; Decoder.More alone accepts a stray '}'.
func TestReadTaskLinesRejectsTrailingJunk(t *testing.T) {
	in := "{\"prompt\":\"x\"}}\n{\"prompt\":\"a\"} {\"prompt\":\"b\"}\n{\"prompt\":\"ok\"}\n"
	got, err := readTaskLines(strings.NewReader(in))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
	if got[0].Err == nil || got[1].Err == nil || got[2].Err != nil {
		t.Errorf("errs = %v / %v / %v, want error, error, nil", got[0].Err, got[1].Err, got[2].Err)
	}
}

func TestParseTaskReadOnlyKeepsToolsOverLineToolsFalse(t *testing.T) {
	lines, err := readTaskLines(strings.NewReader("{\"prompt\":\"p\",\"tools\":false}\n"))
	if err != nil || len(lines) != 1 {
		t.Fatalf("readTaskLines = %d lines, %v", len(lines), err)
	}
	task, err := parseTask(lines[0], "codex", agent.RunOptions{ReadOnly: true}, false, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !task.Req.Tools || !task.Req.ReadOnly {
		t.Errorf("task tools/read-only = %v/%v, want true/true", task.Req.Tools, task.Req.ReadOnly)
	}
}

// Replaying a failed batch leg retains its session and task, applies the exact repair, and leaves siblings alone.
func TestPermissionBatchLegReplay(t *testing.T) {
	failed := failBatchLeg(t)
	code, out, stderr := runCLI(t, "ask", "--retry", failed.ID, "--allow", "Bash(make lint)", "--format", "json")
	if code != 0 {
		t.Fatalf("replay: %d, %s, %s", code, out, stderr)
	}
	replayed := agent.RecentRecords(10)
	if len(replayed) != 3 {
		t.Fatalf("replay ran siblings: records = %d", len(replayed))
	}
	assertReplayedLeg(t, failed, replayed[0].Request)
}

// failBatchLeg runs a two-leg permission batch and saves its newest leg as a refused failure.
func failBatchLeg(t *testing.T) agent.Record {
	t.Helper()
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, cliTask(t, "a", "first"), cliTask(t, "b", "second"))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--agent", "claude", "--fallback", "none", "--allow", "Read", "--deny", "Bash(rm *)")
	if code != 0 {
		t.Fatalf("batch: %d, %s, %s", code, out, stderr)
	}
	recs := agent.RecentRecords(10)
	if len(recs) != 2 {
		t.Fatalf("batch records = %d", len(recs))
	}
	failed := recs[0]
	failed.Status, failed.Error = "error", "permission refused"
	if failed.SessionID == "" {
		t.Fatal("batch leg did not retain its returned session")
	}
	if err := failed.Save(); err != nil {
		t.Fatal(err)
	}
	return failed
}

// assertReplayedLeg checks a replay kept the failed leg's task, session and route and carries the repair beside the deny.
func assertReplayedLeg(t *testing.T, failed agent.Record, got agent.RunOptions) {
	t.Helper()
	if got.SessionID != failed.SessionID || got.Prompt != failed.Request.Prompt || got.Provider != failed.Request.Provider || got.Lead != failed.Request.Lead {
		t.Fatalf("replay lost task/session/route: %+v", got)
	}
	if got.Permissions.Decide("Bash", "make lint") != agent.Allow || got.Permissions.Decide("Bash", "rm x") != agent.Deny {
		t.Fatalf("replay lost exact repair or deny: %+v", got.Permissions)
	}
}
