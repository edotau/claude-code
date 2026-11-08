package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// Each fallback hop emits exactly one retry event with the documented text.
func TestRunWithFallbackEmitsOneRetryPerHop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	first := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("boom")}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	var evs []wire.Event
	sink := sinkFunc(func(ev wire.Event) error { evs = append(evs, ev); return nil })
	c := Chain{Want: "codex", Dispatches: []Dispatch{{Agent: first}, {Agent: second}}}
	if _, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, sink, nil); err != nil {
		t.Fatal(err)
	}
	var retries []string
	for _, ev := range evs {
		if ev.Type == wire.EventRetry {
			retries = append(retries, ev.Text)
		}
	}
	if len(retries) != 1 || retries[0] != "fallback codex → gemini: codex: failed: boom" {
		t.Fatalf("retry events = %q, want one %q", retries, "fallback codex → gemini: codex: failed: boom")
	}
}

type sinkFunc func(wire.Event) error

func (f sinkFunc) Emit(ev wire.Event) error { return f(ev) }

// resultProbe records the on-disk record status at the moment the result event is emitted.
type resultProbe struct {
	id     string
	status string
}

func (p *resultProbe) Emit(ev wire.Event) error {
	if ev.Type == wire.EventResult {
		got, _ := LoadRecord(p.id)
		p.status = got.Status
	}
	return nil
}

// A tail follower exits on a finished record, so the result event must be written before the record leaves running.
func TestDispatchWritesResultBeforeFinishedRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	probe := &resultProbe{id: rec.ID}
	_, err := RunRecorded(context.Background(), &rec, "codex", probe, RunRecordedOpts{},
		func(context.Context, wire.Sink) (wire.Result, error) {
			return wire.Result{Agent: "codex", Status: wire.StatusOK, Answer: "hi"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if probe.status != "running" {
		t.Fatalf("record status at result emit = %q, want running", probe.status)
	}
	if got, _ := LoadRecord(rec.ID); got.Status != wire.StatusOK {
		t.Fatalf("final record status = %q, want %q", got.Status, wire.StatusOK)
	}
}
