package textutil

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryDelayHonoursAndCapsRetryAfter(t *testing.T) {
	now := time.Now()
	for range 50 {
		if d := RetryDelay(1, time.Millisecond, hdr("2"), now); d < 2*time.Second {
			t.Fatalf("delay %v under Retry-After 2s", d)
		}
		if d := RetryDelay(1, time.Millisecond, hdr("86400"), now); d != retryAfterCap {
			t.Fatalf("delay %v, want the %v cap", d, retryAfterCap)
		}
		if d := RetryDelay(30, time.Second, nil, now); d > backoffCap {
			t.Fatalf("backoff %v over cap", d)
		}
	}
}

func TestWaitRetryReturnsContextErrorPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := WaitRetry(ctx, 1, time.Millisecond, hdr("8"), nil); err != context.Canceled || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func hdr(v string) http.Header { return http.Header{"Retry-After": {v}} }

func TestRetryAfterMsWinsAndDeadlineKeepsLastError(t *testing.T) {
	h := http.Header{"Retry-After": {"30"}, "Retry-After-Ms": {"250"}}
	if d, ok := HeaderRetryAfter(h, time.Now()); !ok || d != 250*time.Millisecond {
		t.Fatalf("got %v %v", d, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	err := WaitRetry(ctx, 1, time.Millisecond, hdr("8"), StatusError(503))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v", err)
	}
}
