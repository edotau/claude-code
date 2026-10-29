package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	p := Policy{BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
	for _, c := range []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, time.Second}, {1, 1, 2 * time.Second}, {2, 0, 2 * time.Second}, {3, 1, 8 * time.Second},
		{10, 0, 15 * time.Second}, {10, 1, 30 * time.Second}, {0, 0, time.Second},
	} {
		if got := p.Backoff(c.attempt, c.jitter); got != c.want {
			t.Errorf("Backoff(%d,%v) = %v, want %v", c.attempt, c.jitter, got, c.want)
		}
	}
	if got := p.Delay(time.Minute, 1, 0); got != 30*time.Second {
		t.Errorf("Retry-After must be capped at MaxDelay, got %v", got)
	}
	if got := p.Delay(5*time.Second, 3, 0); got != 5*time.Second {
		t.Errorf("Retry-After must win over backoff, got %v", got)
	}
}

func TestDelayJittersRetryAfter(t *testing.T) {
	p := Policy{BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
	lo, hi := p.Delay(5*time.Second, 1, 0), p.Delay(5*time.Second, 1, 1)
	if lo != 5*time.Second || hi <= lo || hi > p.MaxDelay {
		t.Errorf("Retry-After jitter: lo=%v hi=%v, want lo=5s, lo<hi<=MaxDelay", lo, hi)
	}
	if got := p.Delay(time.Minute, 1, 1); got > p.MaxDelay {
		t.Errorf("jittered Retry-After must stay capped at MaxDelay, got %v", got)
	}
}

func TestDoSharedProviderCooldown(t *testing.T) {
	const cool = 200 * time.Millisecond
	pol := Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Second, Key: "cooldown-test-" + t.Name()}
	failed := make(chan time.Time, 1)
	go Do(context.Background(), pol, func(_ context.Context, attempt int) *Error {
		if attempt == 1 {
			failed <- time.Now()
			return &Error{Agent: "a", Kind: KindRateLimit, RetryAfter: cool, Err: errors.New("429")}
		}
		return nil
	})
	t0 := <-failed
	time.Sleep(20 * time.Millisecond)
	var started time.Time
	if e := Do(context.Background(), pol, func(context.Context, int) *Error { started = time.Now(); return nil }); e != nil {
		t.Fatal(e)
	}
	if gap := started.Sub(t0); gap < cool*3/4 {
		t.Errorf("second Do started %v after the 429, want it held for the %v cooldown", gap, cool)
	}
	other := pol
	other.Key = ""
	t1 := time.Now()
	Do(context.Background(), other, func(context.Context, int) *Error { return nil })
	if time.Since(t1) > cool/2 {
		t.Errorf("an empty Key must not wait on any cooldown")
	}
}

func TestDoKeepsEveryAttemptError(t *testing.T) {
	pol := Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, AttemptTimeout: 50 * time.Millisecond}
	e := Do(context.Background(), pol, func(ctx context.Context, attempt int) *Error {
		switch attempt {
		case 1:
			return &Error{Agent: "a", Kind: KindRateLimit, Err: errors.New("HTTP 429")}
		case 2:
			return &Error{Agent: "a", Kind: KindTransient, Err: errors.New("HTTP 529")}
		}
		<-ctx.Done()
		return &Error{Agent: "a", Kind: KindTransient, Err: ctx.Err()}
	})
	if e == nil || e.Attempts != 3 || e.Kind != KindFatal {
		t.Fatalf("final error = %+v, want a terminal timeout on attempt 3", e)
	}
	if len(e.Earlier) != 2 || e.Earlier[0].Kind != KindRateLimit || e.Earlier[1].Kind != KindTransient ||
		!strings.Contains(e.Earlier[0].Error(), "429") || !strings.Contains(e.Earlier[1].Error(), "529") {
		t.Errorf("Earlier = %+v, want the 429 then the 529", e.Earlier)
	}
}

type statusErr struct{ code int }

func (e statusErr) Error() string   { return fmt.Sprintf("HTTP %d", e.code) }
func (e statusErr) StatusCode() int { return e.code }

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err   error
		kind  Kind
		after time.Duration
	}{
		{statusErr{401}, KindAuth, 0},
		{statusErr{429}, KindRateLimit, 0},
		{statusErr{529}, KindTransient, 0},
		{statusErr{400}, KindFatal, 0},
		{errors.New("API Error: 429 rate_limit_error, retry after 7s"), KindRateLimit, 7 * time.Second},
		{errors.New("Try again in 2 minutes: too many requests"), KindRateLimit, 2 * time.Minute},
		{errors.New("invalid x-api-key"), KindAuth, 0},
		{errors.New("dial tcp: connection refused"), KindTransient, 0},
		{fmt.Errorf("codex: %w", context.DeadlineExceeded), KindTransient, 0},
		{errors.New("parse error at line 4013"), KindFatal, 0},
	} {
		e := Classify("x", c.err, false)
		if e.Kind != c.kind || e.RetryAfter != c.after {
			t.Errorf("Classify(%q) = %v/%v, want %v/%v", c.err, e.Kind, e.RetryAfter, c.kind, c.after)
		}
	}
	if Classify("x", nil, false) != nil {
		t.Error("nil error must classify to nil")
	}
}

func fastPolicy() Policy {
	return Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

func TestDo(t *testing.T) {
	fail := func(kind Kind, progressed bool) *Error {
		return &Error{Kind: kind, Agent: "x", Progressed: progressed}
	}
	cases := []struct {
		name     string
		errs     []*Error // per attempt; nil = success
		recover  func(context.Context) error
		attempts int
		ok       bool
	}{
		{"transient then success", []*Error{fail(KindTransient, false), nil}, nil, 2, true},
		{"rate limit exhausts", []*Error{fail(KindRateLimit, false), fail(KindRateLimit, false), fail(KindRateLimit, false)}, nil, 3, false},
		{"fatal is terminal", []*Error{fail(KindFatal, false), nil}, nil, 1, false},
		{"progress vetoes retry", []*Error{fail(KindTransient, true), nil}, nil, 1, false},
		{"auth without recover is terminal", []*Error{fail(KindAuth, false), nil}, nil, 1, false},
		{"auth with recover retries", []*Error{fail(KindAuth, false), nil}, func(context.Context) error { return nil }, 2, true},
		{"failed recover is terminal", []*Error{fail(KindAuth, false), nil}, func(context.Context) error { return errors.New("no") }, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fastPolicy()
			p.Recover = c.recover
			calls := 0
			e := Do(context.Background(), p, func(_ context.Context, attempt int) *Error {
				calls++
				return c.errs[attempt-1]
			})
			if calls != c.attempts || (e == nil) != c.ok {
				t.Fatalf("calls=%d err=%v, want calls=%d ok=%v", calls, e, c.attempts, c.ok)
			}
		})
	}
}

// MaxAttempts: 0 must behave exactly like 1 — one call, no retry notice, no backoff sleep. BaseDelay
// is a full second so a stray sleep would blow the 100ms deadline below.
func TestDoMaxAttemptsZeroActsAsOne(t *testing.T) {
	p := Policy{MaxAttempts: 0, BaseDelay: time.Second, MaxDelay: time.Second}
	var notified []string
	p.Notify = func(s string) { notified = append(notified, s) }
	calls := 0
	done := make(chan *Error, 1)
	go func() {
		done <- Do(context.Background(), p, func(_ context.Context, attempt int) *Error {
			calls++
			return &Error{Kind: KindTransient, Agent: "x"}
		})
	}()
	select {
	case e := <-done:
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (MaxAttempts:0 means no retry)", calls)
		}
		if e == nil || e.Attempts != 1 {
			t.Errorf("err = %v, want Attempts:1", e)
		}
		if len(notified) != 0 {
			t.Errorf("Notify called %v, want none (nothing to notify about with no retry)", notified)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Do slept/retried with MaxAttempts:0 (should return after exactly one attempt)")
	}
}

func TestDoAttemptTimeoutAndCancel(t *testing.T) {
	p := fastPolicy()
	p.AttemptTimeout = 5 * time.Millisecond
	calls := 0
	e := Do(context.Background(), p, func(ctx context.Context, _ int) *Error {
		calls++
		<-ctx.Done()
		return Classify("x", ctx.Err(), false)
	})
	// An attempt that hits its OWN AttemptTimeout is terminal, not retried: retrying would just
	// burn another full timeout on the same stuck call.
	if calls != 1 || e.Kind != KindFatal || !strings.Contains(e.Error(), "timed out after") || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("attempt-own timeout must not retry: calls=%d err=%v", calls, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	e = Do(ctx, fastPolicy(), func(context.Context, int) *Error {
		calls++
		cancel()
		return &Error{Kind: KindTransient}
	})
	if calls != 1 || e.Kind != KindFatal {
		t.Fatalf("caller cancel must stop the loop: calls=%d err=%v", calls, e)
	}
}

// TestDoAttemptTimeoutFastTransientStillRetried: AttemptTimeout bounds a stuck call, but an attempt
// that fails FAST (a 429, well inside the budget) is still an ordinary retryable transient failure —
// only hitting the attempt's own deadline is terminal.
func TestDoAttemptTimeoutFastTransientStillRetried(t *testing.T) {
	p := fastPolicy()
	p.AttemptTimeout = time.Minute
	calls := 0
	e := Do(context.Background(), p, func(context.Context, int) *Error {
		calls++
		return Classify("x", &httpishError{status: 429}, false)
	})
	if calls != 3 || e.Kind != KindRateLimit {
		t.Fatalf("fast 429 must still retry up to MaxAttempts: calls=%d err=%v", calls, e)
	}
}

// httpishError implements the StatusCode() seam Classify checks before falling back to text matching.
type httpishError struct{ status int }

func (e *httpishError) Error() string   { return fmt.Sprintf("HTTP %d", e.status) }
func (e *httpishError) StatusCode() int { return e.status }

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := DefaultBaseDelay, DefaultMaxDelay
	DefaultBaseDelay, DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { DefaultBaseDelay, DefaultMaxDelay = b, m })
}
