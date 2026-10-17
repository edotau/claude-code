package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// hangRunner blocks until its attempt deadline, like a vendor CLI that buffers all output until exit.
type hangRunner struct{ calls int }

func (h *hangRunner) Name() string     { return "hang" }
func (h *hangRunner) Caps() Caps       { return Caps{Tools: true} }
func (h *hangRunner) Available() error { return nil }
func (h *hangRunner) Run(ctx context.Context, _ Request, _ io.Writer) (Result, error) {
	h.calls++
	<-ctx.Done()
	return Result{}, ctx.Err()
}

// A tools-on attempt that times out may have written files, so it runs once; tools-off still retries.
func TestTimedOutToolsRunIsNotRetried(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := &hangRunner{}
	_, err := Run(context.Background(), h, Request{Prompt: "p", Tools: true, Timeout: 20 * time.Millisecond}, io.Discard, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindFatal || h.calls != 1 {
		t.Fatalf("tools on: calls=%d err=%v", h.calls, err)
	}
	h = &hangRunner{}
	_, _ = Run(context.Background(), h, Request{Prompt: "p", Timeout: 20 * time.Millisecond}, io.Discard, nil)
	if h.calls < 2 {
		t.Errorf("tools off: a timeout is transient and should retry, calls=%d", h.calls)
	}
}
