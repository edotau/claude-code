package roles

import (
	"context"
	"testing"
)

func TestExploreRefusalPreservesAgentIdentity(t *testing.T) {
	seen := fakeStream(t)
	r := Role{Name: "explore", Agent: "gemini"}
	legs := Legs(r, Request{Task: "inspect the repository"})
	if len(legs) != 1 {
		t.Fatalf("want one refused leg, got %+v", legs)
	}
	rep := Run(context.Background(), nil, r, legs, 1, nil)
	if len(*seen) != 0 {
		t.Fatalf("refused explore dispatched: %+v", *seen)
	}
	if rep.Status != "error" || len(rep.Legs) != 1 {
		t.Fatalf("want one failure envelope, got %+v", rep)
	}
	env := rep.Legs[0].Envelope
	if env.Agent != r.Agent {
		t.Errorf("failure envelope agent = %q, want %q", env.Agent, r.Agent)
	}
	const want = "explore on gemini is read-only by prompt only; use codex or copilot (read-only enforced)"
	if env.Status != "error" || env.Error == nil || env.Error.Message != want {
		t.Errorf("failure envelope lost refusal: %+v", env)
	}
}
