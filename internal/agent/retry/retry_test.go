package retry

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/network/egress"
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
		if got := p.backoff(c.attempt, c.jitter); got != c.want {
			t.Errorf("Backoff(%d,%v) = %v, want %v", c.attempt, c.jitter, got, c.want)
		}
	}
	if got := p.delay(time.Minute, 1, 0); got != 30*time.Second {
		t.Errorf("Retry-After must be capped at MaxDelay, got %v", got)
	}
	if got := p.delay(5*time.Second, 3, 0); got != 5*time.Second {
		t.Errorf("Retry-After must win over backoff, got %v", got)
	}
}

func TestDelayJittersRetryAfter(t *testing.T) {
	p := Policy{BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
	lo, hi := p.delay(5*time.Second, 1, 0), p.delay(5*time.Second, 1, 1)
	if lo != 5*time.Second || hi <= lo || hi > p.MaxDelay {
		t.Errorf("Retry-After jitter: lo=%v hi=%v, want lo=5s, lo<hi<=MaxDelay", lo, hi)
	}
	if got := p.delay(time.Minute, 1, 1); got > p.MaxDelay {
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
	if time.Since(t1) > time.Second {
		t.Errorf("an empty Key must not wait on any cooldown")
	}
}

func TestDoKeepsEveryAttemptError(t *testing.T) {
	fastRateLimitFloor(t)
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
	assertEarlierAttemptErrors(t, e)
}

func assertEarlierAttemptErrors(t *testing.T, e *Error) {
	t.Helper()
	if len(e.Earlier) != 2 || e.Earlier[0].Kind != KindRateLimit || e.Earlier[1].Kind != KindTransient ||
		!strings.Contains(e.Earlier[0].Error(), "429") || !strings.Contains(e.Earlier[1].Error(), "529") {
		t.Errorf("Earlier = %+v, want the 429 then the 529", e.Earlier)
	}
}

// statusErr implements the StatusCode() seam Classify checks before falling back to text matching.
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

func fastPolicy(t *testing.T) Policy {
	t.Helper()
	fastRateLimitFloor(t)
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
			p := fastPolicy(t)
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

// A failed Recover reaches the caller's error; it is not reduced to a bare "auth rejected".

// MaxAttempts: 0 acts like 1: one call, no retry notice, no backoff sleep (BaseDelay 1s: a sleep blows the deadline).
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
	case <-time.After(2 * time.Second):
		t.Fatal("Do slept/retried with MaxAttempts:0 (should return after exactly one attempt)")
	}
}

func TestDoAttemptTimeoutAndCancel(t *testing.T) {
	p := fastPolicy(t)
	p.AttemptTimeout = 5 * time.Millisecond
	calls := 0
	e := Do(context.Background(), p, func(ctx context.Context, _ int) *Error {
		calls++
		<-ctx.Done()
		return Classify("x", ctx.Err(), false)
	})
	// An attempt hitting its OWN AttemptTimeout is terminal, not retried.
	if calls != 1 || e.Kind != KindFatal || !strings.Contains(e.Error(), "timed out after") || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("attempt-own timeout must not retry: calls=%d err=%v", calls, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	e = Do(ctx, fastPolicy(t), func(context.Context, int) *Error {
		calls++
		cancel()
		return &Error{Kind: KindTransient}
	})
	if calls != 1 || e.Kind != KindFatal {
		t.Fatalf("caller cancel must stop the loop: calls=%d err=%v", calls, e)
	}
}

// AttemptTimeout bounds a stuck call, but a FAST failure (a 429 inside the budget) is still retried.
func TestDoAttemptTimeoutFastTransientStillRetried(t *testing.T) {
	p := fastPolicy(t)
	p.AttemptTimeout = time.Minute
	calls := 0
	e := Do(context.Background(), p, func(context.Context, int) *Error {
		calls++
		return Classify("x", statusErr{429}, false)
	})
	if calls != 3 || e.Kind != KindRateLimit {
		t.Fatalf("fast 429 must still retry up to MaxAttempts: calls=%d err=%v", calls, e)
	}
}

// A wait that cannot take its permit back returns the cancellation, with the earlier failure kept.
func TestDoCancelledReacquireReturnsCancellation(t *testing.T) {
	p := fastPolicy(t)
	p.MaxAttempts = 3
	p.Idle = func(_ context.Context, wait func()) error { wait(); return context.Canceled }
	e := Do(context.Background(), p, func(context.Context, int) *Error {
		return &Error{Kind: KindRateLimit, Agent: "a", Err: errors.New("HTTP 429")}
	})
	if e == nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", e)
	}
	if len(e.Earlier) != 1 || e.Earlier[0].Kind != KindRateLimit {
		t.Errorf("earlier = %v, want the one rate-limit attempt kept", e.Earlier)
	}
}

// A waiter whose cooldown is extended by another task while it sleeps must keep waiting for the new deadline.
func TestWaitCooldownHonorsExtendedDeadline(t *testing.T) {
	key := "cool-extend-" + t.Name()
	CoolDown(key, 200*time.Millisecond)
	start := time.Now()
	done := make(chan time.Duration)
	go func() { WaitCooldown(context.Background(), key, 5*time.Second); done <- time.Since(start) }()
	time.Sleep(20 * time.Millisecond)
	CoolDown(key, 400*time.Millisecond)
	if got := <-done; got < 400*time.Millisecond {
		t.Errorf("waiter resumed after %v, want it to wait out the extended ~420ms hold", got)
	}
	cooldowns.Lock()
	delete(cooldowns.until, key)
	cooldowns.Unlock()
}

// The final attempt's 429 still publishes its cooldown: a second Do on the same key waits for it.
func TestDoPublishesCooldownOnFinalAttempt(t *testing.T) {
	key := "final-" + t.Name()
	p := Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Second, Key: key}
	Do(context.Background(), p, func(context.Context, int) *Error {
		return &Error{Kind: KindRateLimit, Agent: "a", RetryAfter: 300 * time.Millisecond, Err: errors.New("429")}
	})
	start := time.Now()
	Do(context.Background(), p, func(context.Context, int) *Error { return nil })
	if got := time.Since(start); got < 200*time.Millisecond {
		t.Errorf("second Do started after %v, want it held for the published ~300ms cooldown", got)
	}
}

func assertDispatchSourceGlobals(t *testing.T, p string, banned map[string]bool) {
	t.Helper()
	f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
	if perr != nil {
		t.Fatal(perr)
	}
	for _, d := range f.Decls {
		ast.Inspect(d, func(n ast.Node) bool { return assertDispatchCallGlobals(t, p, n, banned) })
	}
}

func assertDispatchCallGlobals(t *testing.T, p string, n ast.Node, banned map[string]bool) bool {
	t.Helper()
	c, ok := n.(*ast.CallExpr)
	if !ok {
		return true
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return true
	}
	if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name+"."+sel.Sel.Name] {
		t.Errorf("%s: %s.%s in the dispatch path: pass env/dir through childCmd instead", p, id.Name, sel.Sel.Name)
	}
	return true
}

// Guarantee 4: the dispatch path never mutates process-global env or cwd (concurrent tasks would race on it).
func TestNoSetenvOrChdirInDispatchPath(t *testing.T) {
	banned := map[string]bool{"os.Setenv": true, "os.Unsetenv": true, "os.Clearenv": true, "os.Chdir": true}
	var files []string
	for _, dir := range []string{"..", "../../claude", "../../codex", "../../copilot", "../../gemini", "../../opencode"} {
		got, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(got) == 0 {
			t.Fatalf("no sources in %s: %v", dir, err)
		}
		files = append(files, got...)
	}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		assertDispatchSourceGlobals(t, p, banned)
	}
}

// An engine error already wrapped as "<agent>: failed:" is not wrapped again, and a leading agent name is not repeated.
func TestErrorDoesNotRepeatAgentPrefix(t *testing.T) {
	inner := &Error{Agent: "opencode", Kind: KindFatal, Err: errors.New("cannot enforce deny Bash(rm *)")}
	if got, want := (&Error{Agent: "opencode", Kind: KindFatal, Err: inner}).Error(), "opencode: failed: cannot enforce deny Bash(rm *)"; got != want {
		t.Errorf("nested = %q, want %q", got, want)
	}
	if got, want := (&Error{Agent: "codex", Kind: KindFatal, Err: errors.New("codex: cannot enforce x")}).Error(), "codex: failed: cannot enforce x"; got != want {
		t.Errorf("plain = %q, want %q", got, want)
	}
}

// Given millisecond, second and minute hints, Then each unit is honored (ms was once read as minutes).
func TestRetryAfterTextUnits(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"retry after 500ms":      500 * time.Millisecond,
		"try again in 200 ms":    200 * time.Millisecond,
		"retry after 30s":        30 * time.Second,
		"try again in 2 minutes": 2 * time.Minute,
		"retry-after: 3 min":     3 * time.Minute,
		"try again in 6m0s":      6 * time.Minute,
		"try again in 1m30s":     1 * time.Minute,
	} {
		if got := retryAfterText(in); got != want {
			t.Errorf("retryAfterText(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRetryDelayRateLimitFloor(t *testing.T) {
	p := DefaultPolicy()
	for range 100 {
		got := p.retryDelay(&Error{Kind: KindRateLimit}, 1)
		if got < 30*time.Second || got > 45*time.Second {
			t.Fatalf("rate limit delay = %v, want [30s,45s]", got)
		}
	}
}

func TestRetryDelayRateLimitHint(t *testing.T) {
	p := DefaultPolicy()
	got := p.retryDelay(&Error{Kind: KindRateLimit, RetryAfter: time.Second}, 1)
	if got < time.Second || got > 1250*time.Millisecond {
		t.Fatalf("hint delay = %v, want [1s,1.25s]", got)
	}
}

func TestRetryDelayTransientBackoff(t *testing.T) {
	p := DefaultPolicy()
	got := p.retryDelay(&Error{Kind: KindTransient}, 1)
	if got < time.Second || got > 2*time.Second {
		t.Fatalf("transient delay = %v, want [1s,2s]", got)
	}
}

func fastRateLimitFloor(t *testing.T) {
	t.Helper()
	floor := RateLimitFloor
	RateLimitFloor = time.Millisecond
	t.Cleanup(func() { RateLimitFloor = floor })
}

// connectFail is a pre-response connection failure as an engine would report it.
func connectFail() *Error {
	return Classify("dev", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection timed out")}, false)
}

func TestClassifyMarksConnectionFailures(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network is unreachable")}, true},
		{errors.New("Post https://x: net/http: TLS handshake timeout"), true},
		{errors.New("Get https://x: dial tcp 1.2.3.4:443: i/o timeout"), true},
		{errors.New("egress host box: ssh is gone"), true},
		{statusErr{503}, false},
		{statusErr{429}, false},
		{errors.New("API Error: 429 rate limit"), false},
		{context.DeadlineExceeded, false},
		{errors.New("overloaded"), false},
	} {
		if got := Classify("x", c.err, false).Connect; got != c.want {
			t.Errorf("Classify(%q).Connect = %v, want %v", c.err, got, c.want)
		}
	}
}

type pathRun struct {
	calls  []string
	notes  []string
	policy Policy
}

func newPathRun(t *testing.T) *pathRun {
	r := &pathRun{policy: fastPolicy(t)}
	r.policy.Notify = func(s string) { r.notes = append(r.notes, s) }
	return r
}

func (r *pathRun) do(ctx context.Context, fn func(via string) *Error) *Error {
	return Do(ctx, r.policy, func(c context.Context, _ int) *Error {
		via := egress.HostContext(c)
		r.calls = append(r.calls, via)
		return fn(via)
	})
}

func (r *pathRun) alternates() (n int) {
	for _, s := range r.notes {
		if strings.Contains(s, "unreachable via") {
			n++
		}
	}
	return n
}

func TestDoTriesTheOtherPathOnAConnectionFailure(t *testing.T) {
	t.Setenv(egress.EnvHost, "")
	r := newPathRun(t)
	e := r.do(egress.WithHost(t.Context(), "box"), func(via string) *Error {
		if via != "" {
			return connectFail()
		}
		return nil
	})
	if e != nil || fmt.Sprint(r.calls) != "[box ]" || r.alternates() != 1 {
		t.Fatalf("err=%v calls=%q notes=%q; want success on the direct retry with one notice", e, r.calls, r.notes)
	}
	if want := "dev: unreachable via ssh box, trying direct"; r.notes[0] != want {
		t.Fatalf("notice = %q, want %q", r.notes[0], want)
	}

	t.Setenv(egress.EnvHost, "box")
	r = newPathRun(t)
	e = r.do(egress.WithHost(t.Context(), ""), func(via string) *Error {
		if via == "" {
			return connectFail()
		}
		return nil
	})
	if e != nil || fmt.Sprint(r.calls) != "[ box]" || r.notes[0] != "dev: unreachable via direct, trying ssh box" {
		t.Fatalf("err=%v calls=%q notes=%q; want success through the tunnel", e, r.calls, r.notes)
	}
}

func TestDoTriesTheOtherPathOnlyOnce(t *testing.T) {
	t.Setenv(egress.EnvHost, "box")
	r := newPathRun(t)
	e := r.do(t.Context(), func(string) *Error { return connectFail() })
	if e == nil || e.Attempts != 3 || r.alternates() != 1 || len(r.calls) != 4 {
		t.Fatalf("err=%v calls=%q notes=%q; want 1 alternate among 4 calls, then ordinary failure", e, r.calls, r.notes)
	}
	if len(e.Earlier) != 3 {
		t.Fatalf("Earlier = %d, want every failed call kept (3)", len(e.Earlier))
	}
}

func TestDoLeavesOtherFailuresAndNoTunnelAlone(t *testing.T) {
	t.Setenv(egress.EnvHost, "box")
	r := newPathRun(t)
	if e := r.do(t.Context(), func(string) *Error { return &Error{Kind: KindTransient, Agent: "dev", Status: 503} }); e == nil || r.alternates() != 0 {
		t.Fatalf("a 503 is not a connection failure: err=%v notes=%q", e, r.notes)
	}
	t.Setenv(egress.EnvHost, "")
	r = newPathRun(t)
	if e := r.do(t.Context(), func(string) *Error { return connectFail() }); e == nil || r.alternates() != 0 || len(r.calls) != 3 {
		t.Fatalf("no tunnel host: err=%v calls=%q notes=%q; want plain retries", e, r.calls, r.notes)
	}
}
