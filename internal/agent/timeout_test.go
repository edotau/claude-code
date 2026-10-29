package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// hangRunner blocks until its attempt deadline, like a vendor CLI that buffers all output until exit.
type hangRunner struct{ calls int }

func (h *hangRunner) Name() string     { return "hang" }
func (h *hangRunner) Caps() Caps       { return Caps{Tools: Native} }
func (h *hangRunner) Available() error { return nil }
func (h *hangRunner) Run(ctx context.Context, _ Request, _ Sink) (Result, error) {
	h.calls++
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A tools-on attempt that times out may have written files, so it runs once; tools-off also runs once
// now — any attempt that hits its OWN AttemptTimeout is terminal (Do never retries it), regardless of Tools.
func TestTimedOutToolsRunIsNotRetried(t *testing.T) {
	fastDefaultBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := &hangRunner{}
	_, err := Run(context.Background(), h, Request{Prompt: "p", Tools: true, Timeout: 20 * time.Millisecond}, Discard, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindFatal || h.calls != 1 {
		t.Fatalf("tools on: calls=%d err=%v", h.calls, err)
	}
	h = &hangRunner{}
	_, err = Run(context.Background(), h, Request{Prompt: "p", Timeout: 20 * time.Millisecond}, Discard, nil)
	if !errors.As(err, &e) || e.Kind != KindFatal || h.calls != 1 {
		t.Errorf("tools off: attempt's own timeout must not retry either, calls=%d err=%v", h.calls, err)
	}
}
