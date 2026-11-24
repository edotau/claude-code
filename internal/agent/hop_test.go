package agent

import (
	"context"
	"testing"

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
