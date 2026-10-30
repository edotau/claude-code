package agent

import (
	"context"
	"errors"
	"testing"
)

// An explicit --profile leg that fails with the default --fallback all never starts the next leg; the same failure
// without a profile does, so the chain is built to advance.
func TestExplicitLegFailureDoesNotAdvance(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	run := func(provider string) (first, next *fakeRunner, err error) {
		first = &fakeRunner{name: "codex", errs: []error{errors.New("exit status 1")}, emptyAnswer: true}
		next = &fakeRunner{name: "opencode", errs: []error{nil}}
		c := Chain{Want: "codex", Legs: []Leg{{Agent: first}, {Agent: next}}}
		_, err = RunWithFallback(context.Background(), c, Request{Prompt: "p", Provider: provider}, Discard, nil)
		return first, next, err
	}
	if _, next, err := run(""); err != nil || len(next.seen) != 1 {
		t.Fatalf("control (no profile): err=%v, next leg ran %d times, want 1", err, len(next.seen))
	}
	first, next, err := run("openai")
	if err == nil {
		t.Fatal("explicit leg failed but RunWithFallback returned nil")
	}
	if len(first.seen) != 1 || len(next.seen) != 0 {
		t.Fatalf("explicit profile: first ran %d, next ran %d; want 1 and 0 (no silent fallback)", len(first.seen), len(next.seen))
	}
}
