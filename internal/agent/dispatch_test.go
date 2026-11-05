package agent

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
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
		return Classify("x", statusErr{429}, false)
	})
	if calls != 3 || e.Kind != KindRateLimit {
		t.Fatalf("fast 429 must still retry up to MaxAttempts: calls=%d err=%v", calls, e)
	}
}

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := DefaultBaseDelay, DefaultMaxDelay
	DefaultBaseDelay, DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { DefaultBaseDelay, DefaultMaxDelay = b, m })
}

// A wait that cannot take its permit back returns the cancellation, with the earlier failure kept.
func TestDoCancelledReacquireReturnsCancellation(t *testing.T) {
	p := fastPolicy()
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
	coolDown(key, 100*time.Millisecond)
	start := time.Now()
	done := make(chan time.Duration)
	go func() { waitCooldown(context.Background(), key, 5*time.Second); done <- time.Since(start) }()
	time.Sleep(40 * time.Millisecond)
	coolDown(key, 300*time.Millisecond)
	if got := <-done; got < 300*time.Millisecond {
		t.Errorf("waiter resumed after %v, want it to wait out the extended ~340ms hold", got)
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
		// Overlay flags set env at flag parse, before dispatch, so --job relaunches inherit (b868a893).
		if fd, ok := d.(*ast.FuncDecl); ok && (fd.Name.Name == "EnableOverlay" || fd.Name.Name == "EnableOverlayDebug") {
			continue
		}
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
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		assertDispatchSourceGlobals(t, p, banned)
	}
}

// countAccountMarkers counts live marker files in dir whose content is account.
func countAccountMarkers(dir, account string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.TrimSpace(string(b)) == account {
			n++
		}
	}
	return n
}

// samplePeak counts account markers every interval until stop closes and returns the peak.
func samplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(countAccountMarkers(dir, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// Verify-2: 13 batch dispatches plus 5 Agent spawns on one account never exceed the shared cap of 12, every dispatch and
// spawn finishes, and the marker dir is empty at the end. The limiter's caps lift the in-process DefaultPerKey=4
// that RunBatch uses, so the file-backed shared limiter is the binding gate.
func TestBatchAndSpawnsShareTheAccountCap(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	const capA = 12
	lim := inflight.Limits{Base: capA, Total: 1000, Available: []string{account}}

	helper := exec.Command("sleep", "30")
	if err := helper.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	owner := helper.Process.Pid
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })

	l := newLimiter(13, capA)
	l.limits = lim

	stop := make(chan struct{})
	var active, peak atomic.Int32 // holders of a slot right now: batch dispatches plus spawns
	hold := func(d time.Duration) {
		n := active.Add(1)
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(d)
		active.Add(-1)
	}

	var wg sync.WaitGroup
	for range 13 { // 13 batch dispatches: exactly what one dispatch does in l.acquire — take, hold, release
		wg.Go(func() {
			release, err := l.acquire(context.Background(), account)
			if err != nil {
				t.Errorf("dispatch acquire: %v", err)
				return
			}
			hold(150 * time.Millisecond)
			release()
		})
	}
	for i := range 5 { // 5 Agent spawns from a second, live owner pid; retry a refused claim
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			holdSpawnClaim(t, i, owner, account, lim, stop, hold)
		}(i)
	}
	wg.Wait()
	close(stop)

	assertAccountPeakAndMarkers(t, account, peak.Load(), capA)
}

func holdSpawnClaim(t *testing.T, i, owner int, account string, lim inflight.Limits, stop <-chan struct{}, hold func(time.Duration)) {
	t.Helper()
	id := fmt.Sprintf("spawn-%d", i)
	for {
		ok, _ := inflight.TryClaim(inflight.Claim{Owner: owner, ID: id, Account: account, N: 1}, lim)
		if ok {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-stop:
			t.Errorf("spawn %d never claimed", i)
			return
		}
	}
	hold(200 * time.Millisecond)
	inflight.Release(owner, id)
}

func assertAccountPeakAndMarkers(t *testing.T, account string, peak int32, capA int) {
	t.Helper()
	if got := peak; got > int32(capA) {
		t.Errorf("peak concurrent holders on %s = %d, want <= %d", account, got, capA)
	}
	if n := countAccountMarkers(inflight.Dir(), account); n != 0 {
		t.Errorf("%d markers for %s remain after everything released, want 0", n, account)
	}
	if entries, _ := os.ReadDir(inflight.Dir()); len(entries) > 0 {
		names := []string{} // .lock is not a marker and may stay
		for _, e := range entries {
			if _, ok := inflight.Owner(e.Name()); ok {
				names = append(names, e.Name())
			}
		}
		if len(names) > 0 {
			t.Errorf("marker dir not empty at the end: %v", names)
		}
	}
}

// A dispatch waiting on a full shared account holds no local worker: with Jobs=1 a dispatch on a free account still runs,
// and a cancelled wait (or a dispatch cancelled while capacity is free) leaves no marker behind.
func TestSharedWaitHoldsNoLocalWorker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const full, free = "full·acct", "free·acct"
	lim := inflight.Limits{Base: 1, Total: 1000}
	if ok, _ := inflight.TryClaim(inflight.Claim{Owner: os.Getpid(), ID: "other", Account: full}, lim); !ok {
		t.Fatal("seed claim refused")
	}
	l := newLimiter(1, DefaultPerKey)
	l.limits = lim
	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	tried := make(chan struct{}, 1)
	observeRefusedClaim(t, full, tried)
	go func() { _, err := l.acquire(ctx, full); waited <- err }()
	select {
	case <-tried: // the full-account dispatch was refused and is now polling
	case <-time.After(5 * time.Second):
		t.Fatal("full-account dispatch never tried the shared claim")
	}

	assertFreeAccountRuns(t, l, free)
	cancel()
	if err := <-waited; err == nil {
		t.Fatal("cancelled wait returned a permit")
	}
	if _, err := l.acquire(ctx, free); err == nil {
		t.Fatal("an already-cancelled dispatch got a permit while capacity was free")
	}
	if n := countAccountMarkers(inflight.Dir(), full) + countAccountMarkers(inflight.Dir(), free); n != 1 {
		t.Errorf("markers = %d, want only the seed claim", n)
	}
}

func observeRefusedClaim(t *testing.T, full string, tried chan<- struct{}) {
	t.Helper()
	origClaim := sharedTryClaim
	sharedTryClaim = func(c inflight.Claim, lim inflight.Limits) (bool, inflight.Snapshot) {
		ok, err := origClaim(c, lim)
		if c.Account == full && !ok {
			select {
			case tried <- struct{}{}:
			default:
			}
		}
		return ok, err
	}
	t.Cleanup(func() { sharedTryClaim = origClaim })
}

func assertFreeAccountRuns(t *testing.T, l *limiter, free string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		release, err := l.acquire(context.Background(), free)
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("free-account dispatch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dispatch on a free account starved behind one waiting on a full account (Jobs=1)")
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
