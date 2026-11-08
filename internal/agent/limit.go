package agent

// The dispatch limiter: per-account and global concurrency caps, batch slots and fail-fast.

import (
	"context"
	"errors"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// tripFailFast cancels the batch when err ends the task (no fallback follows), before the dispatch's permit is released.
func tripFailFast(ctx context.Context, c Chain, i int, req RunOptions, res wire.Result, err error) {
	trip := retry.FailFastStop(ctx)
	if trip == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if i+1 < len(c.Dispatches) && (i != 0 || !ExplicitBackend(req)) && dispatchUnavailable(c.Dispatches[i].Agent, res, err) {
		return
	}
	trip()
}

// WithBatchRun is ctx for one batch task's dispatches: host-isolated and under l.
func WithBatchRun(ctx context.Context, l *retry.Limiter) context.Context {
	return retry.WithLimiter(withIsolation(ctx), l)
}
