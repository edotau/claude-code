package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
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
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, sink, nil)
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = retry.WithFailFast(ctx, cancel)
			first := &fakeRunner{name: "codex", write: "partial progress", errs: []error{errors.New(message)}}
			second := &fakeRunner{name: "gemini", errs: []error{nil}}
			res, err := RunWithFallback(ctx, chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, wire.Discard, nil)
			if err != nil || ctx.Err() != nil || res.Agent != "gemini" {
				t.Fatalf("fallback = %+v, %v; context = %v; want gemini success without fail-fast cancellation", res, err, ctx.Err())
			}
			if len(first.seen) != 1 || len(second.seen) != 1 {
				t.Fatalf("dispatch calls = %d, %d; want one attempt per dispatch", len(first.seen), len(second.seen))
			}
		})
	}
}
