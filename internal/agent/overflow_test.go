package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/router"
)

const overflowMsg = "pi: prompt is too long: 230000 tokens > 200000 maximum"

var overflowCell = RunOptions{Model: "databricks-claude-sonnet-4-6", CellModel: true, Fallback: router.FallbackAll}

func overflowDisp(agent string) Dispatch {
	return Dispatch{Agent: &fakeRunner{name: agent}, Account: router.ProviderAccount("dev")}
}

// A Sonnet cell whose prompt overflows re-picks a larger window; opencode, which drops [1m], gets a pool model.
func TestPlanOverflowRepicksByWindow(t *testing.T) {
	autoFixture(t)
	over := errors.New(overflowMsg)
	plan := planOverflow(overflowDisp("opencode"), overflowCell, wire.Result{}, overflowCell.Model, over)
	if plan.model == "" || plan.to <= plan.from || plan.refusal != "" || strings.HasSuffix(plan.model, "[1m]") {
		t.Fatalf("opencode plan = %+v, want a larger pool model (it drops [1m])", plan)
	}
	plan = planOverflow(overflowDisp("pi"), overflowCell, wire.Result{}, overflowCell.Model, over)
	if !strings.HasSuffix(plan.model, "[1m]") || plan.to <= plan.from {
		t.Fatalf("pi plan = %+v, want the 1M variant first", plan)
	}
}

// A quota-held first candidate is skipped for the next larger window.
func TestPlanOverflowSkipsHeldModel(t *testing.T) {
	autoFixture(t)
	disp := overflowDisp("opencode")
	first := planOverflow(disp, overflowCell, wire.Result{}, overflowCell.Model, errors.New(overflowMsg))
	now := time.Now()
	if err := markModelDead(disp.Account, first.model, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	next := planOverflow(disp, overflowCell, wire.Result{}, overflowCell.Model, errors.New(overflowMsg))
	if first.model == "" || next.model == "" || next.model == first.model {
		t.Fatalf("held %q, next %q, want a different model", first.model, next.model)
	}
}

// A kept model (typed or --fallback none), the largest window and a rate limit never re-pick.
func TestPlanOverflowRefuses(t *testing.T) {
	autoFixture(t)
	disp, over := overflowDisp("pi"), errors.New(overflowMsg)
	for name, req := range map[string]RunOptions{"none": {Model: overflowCell.Model, CellModel: true, Fallback: router.FallbackNone},
		"typed": {Model: overflowCell.Model, ModelTyped: true, Fallback: router.FallbackAll}} {
		kept := planOverflow(disp, req, wire.Result{}, req.Model, over)
		if kept.model != "" || !strings.Contains(kept.refusal, "larger windows:") {
			t.Fatalf("%s plan = %+v, want a refusal naming larger ids", name, kept)
		}
	}
	top := planOverflow(disp, overflowCell, wire.Result{}, "databricks-gpt-6-1-sol", over)
	if top.model != "" || !strings.Contains(top.refusal, "no model with a larger window") {
		t.Fatalf("largest plan = %+v, want the nothing-larger refusal", top)
	}
	rate := errors.New("429 too many tokens per minute")
	if got := planOverflow(disp, overflowCell, wire.Result{}, overflowCell.Model, rate); got != (overflowPlan{}) {
		t.Fatalf("a rate limit planned an overflow: %+v", got)
	}
}

// overflowRunner answers every run with the model it was asked for.
type overflowRunner struct{ fakeRunner }

func (o *overflowRunner) Run(_ context.Context, req RunOptions, _ wire.Sink) (wire.Result, error) {
	o.seen = append(o.seen, req)
	return wire.Result{Model: req.Model, Answer: "ok"}, nil
}

// repick runs the larger-window model once and records the move, with both windows, on the refused hop.
func TestRepickOverflowRunsLargerWindow(t *testing.T) {
	autoFixture(t)
	eng := &overflowRunner{fakeRunner{name: "pi"}}
	disp := Dispatch{Agent: eng, Account: router.ProviderAccount("dev")}
	var notes []string
	first := errors.New(overflowMsg)
	res, lreq, hops, err := repick(context.Background(), disp, overflowCell, wire.Result{Model: overflowCell.Model}, first,
		wire.Discard, func(s string) { notes = append(notes, s) })
	if err != nil || res.Answer != "ok" || len(eng.seen) != 1 || !strings.HasSuffix(lreq.Model, "[1m]") {
		t.Fatalf("repick = %+v %q err %v seen %d, want one run at the 1M variant", res, lreq.Model, err, len(eng.seen))
	}
	if len(hops) != 1 || len(hops[0].Errors) != 1 || !strings.Contains(hops[0].Errors[0].Message, "context overflow") ||
		len(notes) != 1 || !strings.Contains(notes[0], "200000 tokens") {
		t.Fatalf("hops %+v notes %v, want the reason and both windows", hops, notes)
	}
}
