package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/router"
)

// A policy refusal on native codex moves to a later Gemini hop, never to a hop of its own family or past a typed model.
func TestHopMovesPolicyRefusalToOtherFamily(t *testing.T) {
	setConfigDir(t)
	codex := Dispatch{Agent: &fakeRunner{name: "codex"}, Model: "gpt-6.1-sol", Account: "openai·chatgpt"}
	gemini := Dispatch{Agent: &fakeRunner{name: "gemini"}, Model: "gemini-3-8-flash", Account: "google·login"}
	again := Dispatch{Agent: &fakeRunner{name: "codex"}, Model: "gpt-6.1-sol", Account: "databricks·dev"}
	req := RunOptions{Agent: "codex", Model: "gpt-6.1-sol"}
	ctx := context.Background()

	if !(Chain{Want: "codex", Dispatches: []Dispatch{codex, gemini}}).hopMoves(ctx, 0, req, wire.Result{}, errPolicy) {
		t.Error("a policy refusal did not reach the later gemini hop")
	}
	if (Chain{Want: "codex", Dispatches: []Dispatch{codex, again}}).hopMoves(ctx, 0, req, wire.Result{}, errPolicy) {
		t.Error("a policy refusal moved to a hop of the refused family")
	}
	if !skipsRefusedFamily(codex, again, req, wire.Result{}, errPolicy) || skipsRefusedFamily(codex, gemini, req, wire.Result{}, errPolicy) {
		t.Error("only a hop of the refused family is skipped")
	}
	typed := req
	typed.ModelTyped, typed.Fallback = true, router.FallbackAll
	if (Chain{Want: "codex", Dispatches: []Dispatch{codex, gemini}}).hopMoves(ctx, 0, typed, wire.Result{}, errPolicy) {
		t.Error("a typed model moved to another family")
	}
}

// A typed auto replaces a cell's model, never the model a pi runner hop was validated on.
func TestAutoBeatsCellSparesRunnerHop(t *testing.T) {
	out := RunOptions{AutoModel: true}
	if !autoBeatsCell(out, out, Dispatch{}) {
		t.Error("auto did not replace an own-CLI cell's model")
	}
	if autoBeatsCell(out, out, Dispatch{runFor: "codex"}) {
		t.Error("auto replaced a pi runner hop's picked model")
	}
}

// A client cancel (context.Canceled or a 499) starts no hop, re-pick, family move or account hold.
func TestCancelStartsNoRedispatch(t *testing.T) {
	setConfigDir(t)
	codex := Dispatch{Agent: &fakeRunner{name: "codex"}, Model: "gpt-6.1-sol", Account: "openai·chatgpt", Billing: router.BillSubscription}
	gemini := Dispatch{Agent: &fakeRunner{name: "gemini"}, Model: "gemini-3-8-flash", Account: "google·login"}
	req := RunOptions{Agent: "codex", Model: "gpt-6.1-sol"}
	chain := Chain{Want: "codex", Dispatches: []Dispatch{codex, gemini}}
	ctx := context.Background()
	text := "content filter rate limit 429 prompt is too long: 300000 tokens > 200000 maximum"
	errs := map[string]error{
		"context.Canceled": retry.Classify("codex", fmt.Errorf("%s: %w", text, context.Canceled), false),
		"499":              retry.Classify("codex", &retry.Error{Kind: retry.KindRateLimit, Agent: "codex", Status: retry.StatusClientClosed, Err: errors.New(text)}, false),
	}
	for name, err := range errs {
		if retry.MayRedispatch(err) {
			t.Errorf("%s: MayRedispatch is true", name)
		}
		in := refusedHop{disp: codex, req: req, err: err}
		for path, moved := range map[string]bool{
			"hopMoves":             chain.hopMoves(ctx, 0, req, wire.Result{}, err),
			"HopFailed":            HopFailed(wire.Result{}, err),
			"dispatchUnavailable":  dispatchUnavailableContext(ctx, codex.Agent, wire.Result{}, err),
			"policyRefused":        policyRefused(in, "gpt-6.1-sol"),
			"skipsRefusedFamily":   skipsRefusedFamily(codex, codex, req, wire.Result{}, err),
			"overflowRefusal":      overflowRefusal(err) != "",
			"retryModel":           retryModel(ctx, codex, req, "gpt-6.1-sol", err) != "",
			"overflowFollows":      overflowFollows(chain, 0, req, err),
			"cutOffMidRunContext":  cutOffMidRunContext(ctx, codex.Agent, err),
			"stopsBeforeContinues": !(&fallbackState{err: err, req: req}).stopsBefore(ctx, chain, true),
		} {
			if moved {
				t.Errorf("%s: %s would start another hop", name, path)
			}
		}
		if _, ok := holdable(codex, err); ok {
			t.Errorf("%s: a cancel cools the account", name)
		}
	}
}
