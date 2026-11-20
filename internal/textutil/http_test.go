package textutil

import (
	"context"
	"testing"
	"time"
)

func TestRetryDelayHonoursAndCapsRetryAfter(t *testing.T) {
	now := time.Now()
	for range 50 {
		if d := RetryDelay(1, time.Millisecond, "2", now); d < 2*time.Second {
			t.Fatalf("delay %v under Retry-After 2s", d)
		}
		if d := RetryDelay(1, time.Millisecond, "86400", now); d != retryAfterCap {
			t.Fatalf("delay %v, want the %v cap", d, retryAfterCap)
		}
		if d := RetryDelay(30, time.Second, "", now); d > backoffCap {
			t.Fatalf("backoff %v over cap", d)
		}
	}
}

func TestWaitRetryReturnsContextErrorPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := WaitRetry(ctx, 1, time.Millisecond, "8"); err != context.Canceled || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
