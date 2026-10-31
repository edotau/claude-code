package agent

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// burstRunner emits n text events, then blocks on ctx when hold is set.
type burstRunner struct {
	n    int
	hold bool
	ctx  chan context.Context
}

func (*burstRunner) Name() string     { return "fake" }
func (*burstRunner) Caps() Caps       { return Caps{} }
func (*burstRunner) Available() error { return nil }
func (b *burstRunner) Run(ctx context.Context, _ RunOptions, w Sink) (Result, error) {
	for range b.n {
		_ = w.Emit(Event{Type: EventText, Text: "x"})
	}
	if b.ctx != nil {
		b.ctx <- ctx
	}
	if b.hold {
		<-ctx.Done()
		return Result{}, context.Cause(ctx)
	}
	return Result{Answer: "a"}, nil
}

func streamPrepared(t *testing.T, r Runner) *Prepared {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return &Prepared{Opts: RunOptions{Agent: "fake", Prompt: "p"}, Record: ClaimRecord("fake", RunOptions{Prompt: "p"}, time.Now()), chain: Single(r)}
}

func drainEvents(ch <-chan Event) (results int, last Event) {
	for ev := range ch {
		if ev.Type == EventResult {
			results++
		}
		last = ev
	}
	return results, last
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() <= base {
			return
		}
	}
	t.Fatalf("goroutines = %d, want <= %d: the run leaked", runtime.NumGoroutine(), base)
}

// Given a consumer that reads nothing, When the run emits 3x the buffer, Then the run still finishes and nothing leaks.
func TestStreamConsumerReadsNothingStillFinishes(t *testing.T) {
	p := streamPrepared(t, &burstRunner{n: 3 * streamBuffer})
	base := runtime.NumGoroutine()
	ch := (&Client{}).stream(context.Background(), p)
	waitGoroutines(t, base)
	if len(ch) != streamBuffer {
		t.Errorf("buffered = %d, want a full %d (oldest dropped)", len(ch), streamBuffer)
	}
}

// Given a full buffer with drops, Then the stream still ends in exactly one result, as its last event.
func TestStreamFullBufferDropsStillEndsInOneResult(t *testing.T) {
	var notes []string
	c := &Client{Notify: func(s string) { notes = append(notes, s) }}
	base := runtime.NumGoroutine()
	ch := c.stream(context.Background(), streamPrepared(t, &burstRunner{n: 2 * streamBuffer}))
	waitGoroutines(t, base) // finish with nobody reading, so drops are certain
	results, last := drainEvents(ch)
	if results != 1 || last.Type != EventResult {
		t.Errorf("results = %d, last = %q, want one result last", results, last.Type)
	}
	if len(notes) != 1 {
		t.Errorf("notes = %q, want one dropped-events note", notes)
	}
}

// Given a consumer stalled past streamStall, Then the run's ctx is cancelled with ErrStreamAbandoned.
func TestStreamStalledConsumerCancelsRun(t *testing.T) {
	orig := streamStall
	streamStall = 20 * time.Millisecond
	t.Cleanup(func() { streamStall = orig })
	r := &burstRunner{n: streamBuffer, ctx: make(chan context.Context, 1)}
	ch := (&Client{}).streamWith(context.Background(), "fake", func(ctx context.Context, w Sink) (Result, error) {
		res, err := r.Run(ctx, RunOptions{}, w)
		for ctx.Err() == nil {
			_ = w.Emit(Event{Type: EventText})
			time.Sleep(5 * time.Millisecond)
		}
		return res, err
	})
	ctx := <-r.ctx
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ctx not cancelled by a stalled consumer")
	}
	if !errors.Is(context.Cause(ctx), ErrStreamAbandoned) {
		t.Errorf("cause = %v, want ErrStreamAbandoned", context.Cause(ctx))
	}
	drainEvents(ch)
}

// Given Prepare fails, Then the stream carries one failure result and closes.
func TestStreamPrepareFailureYieldsOneResult(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ch := (&Client{}).Stream(context.Background(), RunOptions{Agent: "no-such-agent", Prompt: "p"})
	results, last := drainEvents(ch)
	if results != 1 || last.Type != EventResult || last.Status != StatusError {
		t.Errorf("results = %d, last = %+v, want one error result", results, last)
	}
}
