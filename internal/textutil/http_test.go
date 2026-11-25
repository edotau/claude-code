package textutil

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryDelayHonoursAndCapsRetryAfter(t *testing.T) {
	now := time.Now()
	for range 50 {
		if d := retryDelay(1, time.Millisecond, hdr("2"), now); d < 2*time.Second {
			t.Fatalf("delay %v under Retry-After 2s", d)
		}
		if d := retryDelay(1, time.Millisecond, hdr("86400"), now); d != retryAfterCap {
			t.Fatalf("delay %v, want the %v cap", d, retryAfterCap)
		}
		if d := retryDelay(30, time.Second, nil, now); d > backoffCap {
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

func TestParseRetryAfterHugeSecondsNeverNegative(t *testing.T) {
	for _, v := range []string{"10000000000", "9223372036854775807"} {
		d, ok := parseRetryAfter(v, time.Now())
		if !ok || d <= 0 {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want positive, true", v, d, ok)
		}
	}
}

func TestIsNonPublicIPCGNATFlag(t *testing.T) {
	cgnat := net.ParseIP("100.64.0.1")
	if IsNonPublicIP(cgnat, false) || !IsNonPublicIP(cgnat, true) {
		t.Errorf("CGNAT must follow blockCGNAT: false=%v true=%v", IsNonPublicIP(cgnat, false), IsNonPublicIP(cgnat, true))
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "ff02::1", "0.1.2.3", "240.0.0.1", "::127.0.0.1", "::ffff:10.0.0.1",
		"192.0.0.1", "192.0.2.1", "198.19.0.1", "198.51.100.1", "203.0.113.1", "64:ff9b::a00:1", "2001::1"} {
		if !IsNonPublicIP(net.ParseIP(s), false) {
			t.Errorf("IsNonPublicIP(%s) = false, want true", s)
		}
	}
	if !IsNonPublicIP(nil, false) || IsNonPublicIP(net.ParseIP("8.8.8.8"), true) {
		t.Error("nil is non-public and 8.8.8.8 public")
	}
}
