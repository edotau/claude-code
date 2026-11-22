package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestFallbackRetrySinkFailureKeepsHops(t *testing.T) {
	vaulttest.Use(t)
	first := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("boom")}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	rejected := errors.New("retry sink rejected event")
	sink := sinkFunc(func(ev wire.Event) error {
		if ev.Type == wire.EventRetry {
			return rejected
		}
		return nil
	})
	res, err := runWithFallback(context.Background(), chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, sink, nil)
	if !errors.Is(err, rejected) {
		t.Fatalf("fallback error = %v, want the retry sink error", err)
	}
	if len(first.seen) != 1 || len(second.seen) != 0 {
		t.Fatalf("dispatch calls = %d, %d; want only codex to have run", len(first.seen), len(second.seen))
	}
	if len(res.Hops) != 1 || res.Hops[0].Agent != "codex" {
		t.Fatalf("hops = %+v, want the attempted codex hop", res.Hops)
	}
	if got := wire.RanOn(res); got == "" || !strings.Contains(got, "codex") {
		t.Fatalf("RanOn = %q, want the attempted codex dispatch", got)
	}
}

func TestFailFastPreservesProgressedFallback(t *testing.T) {
	for _, message := range []string{"429 Too Many Requests", "insufficient_quota: you exceeded your current quota"} {
		t.Run(message, func(t *testing.T) {
			vaulttest.Use(t)
			fastDefaultBackoff(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = retry.WithFailFast(ctx, cancel)
			first := &fakeRunner{name: "codex", write: "partial progress", errs: []error{errors.New(message)}}
			second := &fakeRunner{name: "gemini", errs: []error{nil}}
			res, err := runWithFallback(ctx, chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, wire.Discard, nil)
			if err != nil || ctx.Err() != nil || res.Agent != "gemini" {
				t.Fatalf("fallback = %+v, %v; context = %v; want gemini success without fail-fast cancellation", res, err, ctx.Err())
			}
			if len(first.seen) != 1 || len(second.seen) != 1 {
				t.Fatalf("dispatch calls = %d, %d; want one attempt per dispatch", len(first.seen), len(second.seen))
			}
		})
	}
}

// modelRunner returns errs[model] for that model and answers on any other.
type modelRunner struct {
	fakeRunner
	errs map[string]error
}

func (m *modelRunner) Run(_ context.Context, req RunOptions, _ wire.Sink) (wire.Result, error) {
	m.seen = append(m.seen, req)
	if err := m.errs[req.Model]; err != nil {
		return wire.Result{}, err
	}
	return wire.Result{Answer: "a"}, nil
}

// A model-specific 429 that also triggers the repick benches the account only when the repick fails too.
func TestRepickedRateLimitStillMarksAccount(t *testing.T) {
	// Terminal skips Run's in-run 429 backoff (three attempts, ~10s); the repick path is the same either way
	rl := &retry.Error{Kind: retry.KindRateLimit, Agent: "fake", Terminal: true, Err: errors.New("429 rate limit: ENDPOINT_NOT_FOUND")}
	for name, repickErr := range map[string]error{"repick answers": nil, "repick fails": errors.New("boom: unrelated failure")} {
		t.Run(name, func(t *testing.T) {
			fastDefaultBackoff(t)
			fallbackConfigDir(t)
			r := &modelRunner{name: "fake", caps: Caps{Model: Native}, errs: map[string]error{"m-a": rl, "m-b": repickErr}}
			c := chainOf([]Engine{r}, "m-a")
			c.Dispatches[0].keepCaller, c.Dispatches[0].Account, c.Dispatches[0].Billing = false, "openai·chatgpt", router.BillSubscription
			c.Dispatches[0].servedRepick = func(string) string { return "m-b" }
			_, err := runWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, wire.Discard, nil)
			if len(r.seen) != 2 || (err == nil) != (repickErr == nil) {
				t.Fatalf("err=%v runs=%d; want the repick to run and return its own outcome", err, len(r.seen))
			}
			if marked := loadState()["openai·chatgpt"].Status(time.Now()) == router.StateExhausted; marked != (repickErr != nil) {
				t.Errorf("account benched = %v, want %v", marked, repickErr != nil)
			}
		})
	}
}

func TestDispatchRequestCellModelOnEveryHop(t *testing.T) {
	for _, first := range []bool{true, false} {
		if req := dispatchRequest(RunOptions{}, Dispatch{Model: "gpt-6-sol"}, first); !req.CellModel {
			t.Errorf("first=%v: a router cell model must re-resolve against its row", first)
		}
		row := route.Route{Provider: "databricks", Profile: "dev"}
		if req := dispatchRequest(RunOptions{}, Dispatch{Model: "opus", Route: row}, first); req.CellModel || req.CellRoute != row || req.Model != "opus" {
			t.Errorf("first=%v: a cell pinned to its row is not a bare cell model: %+v", first, req)
		}
	}
}

// A typed [1m] survives the cell's bare model: the hop's own row decides the window (no table fits dispatchRequest→resolveIn).
func TestDispatchRequestTypedOneMReachesTarget(t *testing.T) {
	vaulttest.Use(t)
	reg := routeRegistry(t)
	for _, c := range []struct {
		name, typed, cell, row string
		want                   int
	}{
		{"typed on a 1M row", "claude-sonnet-4-6[1m]", "databricks-claude-sonnet-4-6", "dev", models.ContextWindow1M},
		{"untyped is standard", "claude-sonnet-4-6", "databricks-claude-sonnet-4-6", "dev", models.ContextWindowStd},
		{"typed on a row that cannot serve 1M", "claude-sonnet-4-6[1m]", "claude-sonnet-4.6", "aueric_boehcp", models.ContextWindowStd},
	} {
		d := Dispatch{Model: c.cell, Route: route.Route{Provider: reg.Providers[c.row].Provider, Profile: c.row}}
		req := dispatchRequest(RunOptions{Agent: "pi", Model: c.typed, ModelTyped: true}, d, true)
		_, tgt, err := resolveIn(reg, req)
		if err != nil || tgt.ContextTokens != c.want || tgt.Model != c.cell && c.row == "dev" {
			t.Errorf("%s: window %d model %q err %v, want %d", c.name, tgt.ContextTokens, tgt.Model, err, c.want)
		}
	}
}
