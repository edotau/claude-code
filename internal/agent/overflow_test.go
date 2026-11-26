package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

const overflowMsg = "pi: prompt is too long: 230000 tokens > 200000 maximum"

var overflowCell = RunOptions{Model: "databricks-claude-sonnet-4-6", CellModel: true, Fallback: route.FallbackAll}

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

// A larger window the prompt fits but leaves no room for the reply is not a re-pick.
func TestLargerWindowKeepsReplyRoom(t *testing.T) {
	autoFixture(t)
	p := loadRegistryWarn().Row("dev")
	cand := "databricks-gpt-6-1-sol"
	window := providers.Window(p, cand)
	pool := []string{cand}
	if got := router.LargerWindow(p, overflowCell.Model, pool, window/2, false); len(got) != 1 {
		t.Fatalf("half-window prompt: picks %v, want %s", got, cand)
	}
	if got := router.LargerWindow(p, overflowCell.Model, pool, window-1000, false); len(got) != 0 {
		t.Fatalf("prompt leaving 1000 tokens for the reply: picks %v, want none", got)
	}
}

// An overflow naming no token count takes the largest window, not the first larger one.
func TestPlanOverflowUnknownSizeTakesLargest(t *testing.T) {
	autoFixture(t)
	disp := overflowDisp("opencode")
	known := planOverflow(disp, overflowCell, wire.Result{}, overflowCell.Model, errors.New(overflowMsg))
	blind := planOverflow(disp, overflowCell, wire.Result{}, overflowCell.Model, errors.New("pi: input length exceeds context length"))
	if blind.model == "" || blind.to < known.to {
		t.Fatalf("unknown-size plan = %+v, known %+v, want the largest window", blind, known)
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
	for name, req := range map[string]RunOptions{"none": {Model: overflowCell.Model, CellModel: true, Fallback: route.FallbackNone},
		"typed": {Model: overflowCell.Model, ModelTyped: true, Fallback: route.FallbackAll}} {
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

// A context overflow on the last-resort (local) row is terminal: no re-pick, a refusal naming the row.
func TestPlanOverflowLastResortIsTerminal(t *testing.T) {
	reg, _, _ := autoFixture(t)
	local := providers.LastResort(reg)
	if local == nil {
		t.Fatal("fixture: the defaults carry no last_resort row")
	}
	disp := Dispatch{Agent: &fakeRunner{name: "pi"}, Account: local.Account()}
	ran := "qwen3.8-27b:gguf:q6"
	for _, err := range []error{errors.New(overflowMsg), &retry.Error{Kind: retry.KindContext, Agent: "pi", Err: errors.New("overflow")}} {
		plan := planOverflow(disp, overflowCell, wire.Result{}, ran, err)
		if plan.model != "" || !strings.Contains(plan.refusal, "last-resort row "+local.Name) {
			t.Fatalf("%v: plan = %+v, want the terminal last-resort refusal", err, plan)
		}
	}
	typed := &retry.Error{Kind: retry.KindContext, Agent: "pi", Err: errors.New("too many tokens")}
	if got := planOverflow(overflowDisp("pi"), overflowCell, wire.Result{}, overflowCell.Model, typed); got.model == "" {
		t.Fatalf("a typed KindContext on a hosted row must re-pick, past the per-minute veto: %+v", got)
	}
}

// A cancelled or 499 request never re-picks, whatever overflow wording it carries.
func TestPlanOverflowNeverAfterClientCancel(t *testing.T) {
	reg, _, _ := autoFixture(t)
	row := reg.Row("dev")
	ran := providers.SpellPin(row, rotationPool("opencode", "", row, false, time.Now())[0])
	for name, err := range map[string]error{
		"cancel": fmt.Errorf("%s: %w", overflowMsg, context.Canceled),
		"499":    &retry.Error{Kind: retry.KindFatal, Agent: "pi", Status: retry.StatusClientClosed, Err: errors.New(overflowMsg)},
	} {
		if got := planOverflow(overflowDisp("pi"), overflowCell, wire.Result{}, overflowCell.Model, err); got != (overflowPlan{}) {
			t.Errorf("%s: planned %+v, want no overflow plan", name, got)
		}
		disp := Dispatch{Agent: &pickRefuser{}, Account: row.Account()}
		if got := retryModel(context.Background(), disp, RunOptions{Model: AutoModel}, ran, err); got != "" {
			t.Errorf("%s: auto retry = %q, want none", name, got)
		}
	}
}

// An auto ask re-picks past a typed KindContext with no vendor wording (a stream-error code): its Error() reads back.
func TestRetryModelAutoRepicksTypedContext(t *testing.T) {
	reg, _, _ := autoFixture(t)
	row := reg.Row("dev")
	ran := providers.SpellPin(row, rotationPool("opencode", "", row, false, time.Now())[0])
	disp := Dispatch{Agent: &pickRefuser{}, Account: row.Account()}
	coded := retry.Classify("opencode", codedOverflow{}, false)
	if coded.Kind != retry.KindContext {
		t.Fatalf("fixture: %v classified %v", coded, coded.Kind)
	}
	if got := retryModel(context.Background(), disp, RunOptions{Model: AutoModel}, ran, coded); got != AutoModel {
		t.Fatalf("auto retry = %q, want the auto re-pick", got)
	}
}

// codedOverflow is a stream error whose only overflow evidence is its typed code.
type codedOverflow struct{}

func (codedOverflow) Error() string     { return "stream error" }
func (codedOverflow) ErrorCode() string { return "context_length_exceeded" }

// A second overflow after the one window re-pick is a context budget end: no further hop re-sends it.
func TestRepickSecondOverflowIsContextBudget(t *testing.T) {
	autoFixture(t)
	eng := &failRunner{fakeRunner{name: "pi"}, errors.New(overflowMsg)}
	disp := Dispatch{Agent: eng, Account: router.ProviderAccount("dev")}
	_, _, _, err := repick(context.Background(), disp, overflowCell, wire.Result{Model: overflowCell.Model}, errors.New(overflowMsg), wire.Discard, nil)
	if retry.BudgetOf(err) != retry.BudgetContext || retry.MayRedispatch(err) || len(eng.seen) != 1 {
		t.Fatalf("budget %q redispatch %v runs %d: want context, no re-send, one re-pick", retry.BudgetOf(err), retry.MayRedispatch(err), len(eng.seen))
	}
}

// failRunner fails every run with err.
type failRunner struct {
	fakeRunner
	err error
}

func (f *failRunner) Run(_ context.Context, req RunOptions, _ wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	return wire.Result{Model: req.Model}, f.err
}
