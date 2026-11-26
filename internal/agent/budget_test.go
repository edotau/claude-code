package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

func TestIdleTimeoutBudgetKinds(t *testing.T) {
	trip := func(setup func(*IdleTimeout)) error {
		w := NewIdleTimeout("x", time.Hour, nil)
		setup(w)
		w.Kill()
		return w.Err()
	}
	if err := trip(func(*IdleTimeout) {}); retry.IsBudget(err) || !errors.Is(err, ErrStalled) || !retry.MayRedispatch(err) {
		t.Errorf("stall before any output or tool call stays transport: %v", err)
	}
	if err := trip(func(w *IdleTimeout) { w.Spoke() }); retry.BudgetOf(err) != retry.BudgetIdle || retry.MayRedispatch(err) {
		t.Errorf("silence after output is an idle budget: %v", err)
	}
	if err := trip(func(w *IdleTimeout) { w.ToolCall() }); retry.BudgetOf(err) != retry.BudgetIdle {
		t.Errorf("stall after a tool call is an idle budget: %v", err)
	}
}

func TestBudgetRecordRoundTripAndHandback(t *testing.T) {
	wrote := []string{"/lane/a.go", "/lane/b.go"}
	rec := Record{ID: "d-1", Agent: "pi", Status: "error", Tree: "/lane", Wrote: &wrote}
	tl := &tally{Sink: wire.Discard}
	tl.Emit(wire.Event{Type: wire.EventToolUse, Tool: &wire.ToolRef{Name: "bash"}})
	err := retry.WithBudget(errors.New("boom"), retry.BudgetOutput)
	rec.stopBudget(err, tl)
	rec.Finish(wire.Result{}, err, time.Second)
	b, merr := json.Marshal(rec)
	if merr != nil {
		t.Fatal(merr)
	}
	var back Record
	if uerr := json.Unmarshal(b, &back); uerr != nil {
		t.Fatal(uerr)
	}
	if back.BudgetKind != "output" || back.StoppedAt == nil || back.StoppedAt.ToolCalls != 1 || back.StoppedAt.Event != "tool_use bash" ||
		len(back.Changed) != 2 {
		t.Fatalf("round trip lost the budget fields: %+v %+v", back, back.StoppedAt)
	}
	hint := ResumeHint(back)
	for _, want := range []string{"take over lane /lane", "/lane/a.go", "stopped at tool_use bash", "restart this one task"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hand-back %q lacks %q", hint, want)
		}
	}
	if strings.Contains(hint, "--retry") {
		t.Errorf("hand-back still offers a re-send: %q", hint)
	}
	var old Record
	if json.Unmarshal([]byte(`{"id":"o-1","status":"error"}`), &old) != nil || old.BudgetKind != "" || strings.Contains(ResumeHint(old), "take over") {
		t.Errorf("an old record must read as no budget end: %+v", old)
	}
	env := wire.NewEnvelope(wire.Result{}, err, "d-1")
	back.stampBudget(&env)
	if env.BudgetKind != "output" || env.Lane != "/lane" || len(env.Changed) != 2 || env.StoppedAt == nil {
		t.Errorf("envelope lacks the budget fields: %+v", env)
	}
}

func TestBudgetCutoffNotMovedOrFiled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := Record{ID: "d-2", Agent: "pi", Request: RunOptions{Prompt: "p"}}
	err := retry.WithBudget(context.DeadlineExceeded, retry.BudgetDeadline)
	rec.Finish(wire.Result{}, err, 0)
	if rec.Cutoff != CutoffDeadline || rec.BudgetKind != "deadline" {
		t.Fatalf("cutoff %q budget %q", rec.Cutoff, rec.BudgetKind)
	}
	if nerr := rec.NoteCutoff(time.Now()); nerr != nil || len(readCutoffs()) != 0 {
		t.Fatalf("a budget end filed a cutoff that steers a resend: %v %+v", nerr, readCutoffs())
	}
	if CutoffOf(&retry.AdmissionError{Account: "a", Err: context.DeadlineExceeded}) != "" {
		t.Error("an admission wait read as a deadline cutoff")
	}
}

func TestPauseAloneIsNotAToolCall(t *testing.T) {
	w := NewIdleTimeout("x", time.Hour, nil)
	w.Pause() // a retry wait or compaction hold, not a tool
	w.Kill()
	if err := w.Err(); retry.IsBudget(err) || !retry.MayRedispatch(err) {
		t.Fatalf("a held window with no tool call is a transport stall: %v", err)
	}
}

func TestChangedFilesKnownEmptyIsNotTheDirtyTree(t *testing.T) {
	none := []string{}
	if got := (Record{Tree: t.TempDir(), Wrote: &none}).changedFiles(); len(got) != 0 {
		t.Fatalf("a run that observed no writes was given %v", got)
	}
}

func TestLateBudgetMarkKeepsStopPoint(t *testing.T) {
	wrote := []string{"/lane/a.go"}
	rec := Record{ID: "d-3", Tree: "/lane", Wrote: &wrote}
	rec.stopBudget(errors.New("plain"), &tally{Sink: wire.Discard}) // the run ended without a budget; a settle step adds one later
	rec.Finish(wire.Result{}, retry.WithBudget(errors.New("late"), retry.BudgetContract), 0)
	if rec.BudgetKind != "contract" || rec.StoppedAt == nil || len(rec.Changed) != 1 {
		t.Fatalf("late mark lost the stop point: %+v %+v", rec, rec.StoppedAt)
	}
}
