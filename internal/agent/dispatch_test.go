package agent

import (
	"context"
	"errors"
	"fmt"
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
	if got := p.Delay(5*time.Second, 3, 1); got != 5*time.Second {
		t.Errorf("Retry-After must win over backoff, got %v", got)
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
	if calls != 3 || e.Kind != KindTransient {
		t.Fatalf("per-attempt timeout should retry as transient: calls=%d err=%v", calls, e)
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

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := DefaultBaseDelay, DefaultMaxDelay
	DefaultBaseDelay, DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { DefaultBaseDelay, DefaultMaxDelay = b, m })
}
