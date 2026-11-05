package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Kind is the failure class; it alone decides retryability.
type Kind int

const (
	KindNone      Kind = iota
	KindAuth           // credential rejected; retried only when a Recover can refresh it
	KindRateLimit      // 429 / quota
	KindTransient      // network, 5xx, overload, per-attempt timeout
	KindFatal          // everything else: bad request, missing binary, the agent's own verdict
	KindQuota          // an exhausted account (402, monthly/plan quota): never retried, the router marks it
)

func (k Kind) String() string {
	return [...]string{"ok", "auth rejected", "rate limited", "transient failure", "failed", "quota exhausted"}[k]
}

// Error is a classified agent failure.
type Error struct {
	Kind       Kind
	Agent      string
	Status     int // HTTP status when known
	RetryAfter time.Duration
	Progressed bool // output already reached the stream: never retried
	Attempts   int
	AuthReason string // set on a KindAuth whose Recover failed with a login/rejected verdict: mark the account, move on
	Err        error
	Earlier    []*Error // the attempts before this one, oldest first; Do sets it on the error it returns
	terminal   bool     // a state check refused the attempt: retrying cannot help
}

// progressed is true when err is an *Error whose output already reached the stream.
func progressed(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Progressed
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Agent, e.Kind)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Attempts > 1 {
		msg += fmt.Sprintf(" (after %d attempts)", e.Attempts)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// attemptMessage is Error() as one attempt saw it, without the "(after N attempts)" tally.
func (e *Error) attemptMessage() string {
	one := *e
	one.Attempts = 0
	return one.Error()
}

// Retryable is true for rate-limit and transient failures that have not streamed output.
func (e *Error) Retryable() bool {
	return !e.Progressed && !e.terminal && (e.Kind == KindRateLimit || e.Kind == KindTransient || e.Kind == KindAuth)
}

// KindForStatus maps an HTTP status to a Kind; 0 means the transport never completed.
func KindForStatus(status int) Kind {
	switch {
	case status >= 200 && status < 300:
		return KindNone
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusPaymentRequired:
		return KindQuota
	case status == http.StatusTooManyRequests:
		return KindRateLimit
	case status == 0, status == http.StatusRequestTimeout, status >= 500:
		return KindTransient
	}
	return KindFatal
}

// Bare codes are \b-anchored so "line 4013" never reads as a 401.
var (
	rateRE       = regexp.MustCompile(`\b429\b|rate.?limit|too many requests|quota exceeded|resource_exhausted`)
	authRE       = regexp.MustCompile(`\b40[13]\b|unauthorized|forbidden|invalid.?api.?key|authentication|not logged in|invalid x-api-key`)
	transientRE  = regexp.MustCompile(`\b50[0234]\b|\b529\b|overloaded|timed? ?out|deadline exceeded|connection (reset|refused)|no such host|\beof\b|econnreset|etimedout|tls handshake|broken pipe`)
	retryAfterRE = regexp.MustCompile(`(?:retry[- ]after|try again in)[:\s]+(\d+)\s*(m|min|minutes?)?`)
)

// Classify maps an error (or a failed child's stderr tail) to a Kind; typed status errors win over text.
func Classify(agent string, err error, progressed bool) *Error {
	if err == nil {
		return nil
	}
	if done, ok := errors.AsType[*Error](err); ok {
		done.Progressed = done.Progressed || progressed
		return done
	}
	e := &Error{Kind: KindFatal, Agent: agent, Progressed: progressed, Err: err}
	var st interface{ StatusCode() int }
	if errors.As(err, &st) {
		e.Status = st.StatusCode()
		e.Kind = KindForStatus(e.Status)
		if e.Kind == KindRateLimit && providers.IsQuota(err.Error()) {
			e.Kind = KindQuota
		}
	} else {
		e.Kind, e.RetryAfter = classifyText(err)
	}
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) && ra.RetryAfter() > 0 {
		e.RetryAfter = ra.RetryAfter()
	}
	return e
}

func classifyText(err error) (Kind, time.Duration) {
	low := strings.ToLower(err.Error())
	kind := KindFatal
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF):
		kind = KindTransient
	case providers.IsQuota(low):
		kind = KindQuota
	case rateRE.MatchString(low):
		kind = KindRateLimit
	case authRE.MatchString(low):
		kind = KindAuth
	case transientRE.MatchString(low):
		kind = KindTransient
	}
	var delay time.Duration
	if m := retryAfterRE.FindStringSubmatch(low); m != nil {
		n, _ := strconv.Atoi(m[1])
		delay = time.Duration(n) * time.Second
		if m[2] != "" {
			delay = time.Duration(n) * time.Minute
		}
	}
	return kind, delay
}

// Policy bounds one retry loop.
type Policy struct {
	MaxAttempts    int
	BaseDelay      time.Duration
	MaxDelay       time.Duration
	AttemptTimeout time.Duration // 0 = none
	// Recover refreshes a rejected credential; KindAuth is retried only when it is set and succeeds.
	Recover func(context.Context) error
	Notify  func(string)
	// Idle runs wait with the caller's concurrency permit released and retaken after; nil just runs wait.
	Idle func(ctx context.Context, wait func()) error
	// Key names the provider account whose rate limit this loop shares with other loops; "" shares nothing.
	Key string
}

// DefaultBaseDelay and DefaultMaxDelay are DefaultPolicy's backoff bounds; tests shrink them.
var (
	DefaultBaseDelay = 2 * time.Second
	DefaultMaxDelay  = 30 * time.Second
)

// DefaultPolicy suits slow, expensive agent runs: few attempts, real spacing.
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 3, BaseDelay: DefaultBaseDelay, MaxDelay: DefaultMaxDelay, AttemptTimeout: 10 * time.Minute}
}

// Backoff is the equal-jitter wait after attempt (1-based); jitter in [0,1] keeps it pure for tests.
func (p Policy) Backoff(attempt int, jitter float64) time.Duration {
	window := float64(p.BaseDelay) * math.Pow(2, float64(max(attempt, 1)-1))
	if window > float64(p.MaxDelay) || math.IsInf(window, 0) {
		window = float64(p.MaxDelay)
	}
	return time.Duration(window/2 + jitter*window/2)
}

// Delay prefers the upstream's Retry-After plus jitter (so tasks given the same one wake apart), capped at MaxDelay.
func (p Policy) Delay(retryAfter time.Duration, attempt int, jitter float64) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter+time.Duration(jitter*float64(min(retryAfter/4, p.BaseDelay))), p.MaxDelay)
	}
	return p.Backoff(attempt, jitter)
}

// cooldowns is when each provider key may be tried again, shared by every Do in the process.
var cooldowns = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// coolDown holds key back for d; a shorter hold never replaces a longer one.
func coolDown(key string, d time.Duration) {
	cooldowns.Lock()
	defer cooldowns.Unlock()
	if t := time.Now().Add(d); t.After(cooldowns.until[key]) {
		cooldowns.until[key] = t
	}
}

// waitCooldown blocks until key's cooldown ends, for at most maxWait, or until ctx is done.
func waitCooldown(ctx context.Context, key string, maxWait time.Duration) {
	if key == "" {
		return
	}
	var limit time.Time
	if maxWait > 0 {
		limit = time.Now().Add(maxWait)
	}
	for {
		cooldowns.Lock()
		wait := time.Until(cooldowns.until[key]) // re-read: another task may have extended the hold while we slept
		cooldowns.Unlock()
		if !limit.IsZero() {
			wait = min(wait, time.Until(limit))
		}
		if wait <= 0 {
			return
		}
		settle(ctx) // a cooling task holds no slot, so a task on another account can start
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// cooling is true while key's cooldown has time left.
func cooling(key string) bool {
	cooldowns.Lock()
	defer cooldowns.Unlock()
	return key != "" && time.Until(cooldowns.until[key]) > 0
}

func (p Policy) idle(ctx context.Context, wait func()) error {
	if p.Idle == nil {
		wait()
		return nil
	}
	return p.Idle(ctx, wait)
}

// Do runs fn under the policy; fn gets a per-attempt context and the 1-based attempt number.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context, attempt int) *Error) *Error {
	p.MaxAttempts = max(p.MaxAttempts, 1)
	var last *Error
	var earlier []*Error
	fail := func() *Error { // every attempt before the last rides on the error returned
		last.Earlier = earlier
		return last
	}
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if last != nil {
			earlier = append(earlier, withoutEarlier(last))
		}
		if cooling(p.Key) { // before the attempt's own timeout; the wait holds no permit, first attempt included
			if err := p.idle(ctx, func() { waitCooldown(ctx, p.Key, p.MaxDelay) }); err != nil {
				return abandoned(err, attempt, last, earlier, false)
			}
		}
		var attemptErr error
		last, attemptErr = p.runAttempt(ctx, attempt, fn)
		if last == nil {
			return nil
		}
		last.Attempts = attempt
		if p.terminal(ctx, last, attemptErr) {
			return fail()
		}
		wait := p.retryDelay(last, attempt)
		if !p.mayRetry(ctx, last, attempt) {
			return fail()
		}
		p.notifyRetry(last, attempt, wait)
		if ierr := p.backoffWait(ctx, wait); ierr != nil {
			return abandoned(ierr, attempt, last, earlier, true)
		}
		if ctx.Err() != nil {
			return fail()
		}
	}
	return fail()
}

func (p Policy) notifyRetry(last *Error, attempt int, wait time.Duration) {
	if p.Notify != nil {
		p.Notify(fmt.Sprintf("%s: %s (attempt %d/%d), retrying in %s", last.Agent, last.Kind, attempt, p.MaxAttempts, wait.Round(100*time.Millisecond)))
	}
}

func (p Policy) retryDelay(last *Error, attempt int) time.Duration {
	wait := p.Delay(last.RetryAfter, attempt, rand.Float64())
	if p.Key != "" && (last.Kind == KindRateLimit || last.Kind == KindTransient) {
		coolDown(p.Key, wait) // Other loops back off too, even when this one gives up.
	}
	return wait
}

// withoutEarlier is a copy of e with its own Earlier chain cut.
func withoutEarlier(e *Error) *Error {
	snap := *e
	snap.Earlier = nil
	return &snap
}

// abandoned is the error of a wait that could not take its permit back: the cancellation, earlier failures kept.
func abandoned(err error, attempt int, last *Error, earlier []*Error, pending bool) *Error { // pending: last is not yet in earlier
	e := &Error{Kind: KindFatal, Err: err, Attempts: attempt, Earlier: earlier}
	if last != nil {
		e.Agent = last.Agent
		if pending {
			e.Earlier = append(slices.Clone(earlier), withoutEarlier(last))
		}
	}
	return e
}

// runAttempt runs fn once under AttemptTimeout and returns its error plus the attempt context's own.
func (p Policy) runAttempt(ctx context.Context, attempt int, fn func(ctx context.Context, attempt int) *Error) (*Error, error) {
	actx, cancel := ctx, context.CancelFunc(func() {})
	if p.AttemptTimeout > 0 {
		actx, cancel = context.WithTimeout(ctx, p.AttemptTimeout)
	}
	e := fn(actx, attempt)
	attemptErr := actx.Err()
	cancel()
	return e, attemptErr
}

// terminal makes last fatal when the caller cancelled or the attempt ran out its own timeout.
func (p Policy) terminal(ctx context.Context, last *Error, attemptErr error) bool {
	if ctx.Err() != nil {
		last.Kind = KindFatal // the caller cancelled: never retry
		return true
	}
	// The attempt ran out its own AttemptTimeout (not the parent ctx): retrying would just
	// burn another full timeout for the same stuck call, so make it terminal and say so.
	if p.AttemptTimeout > 0 && errors.Is(attemptErr, context.DeadlineExceeded) {
		last.Kind = KindFatal
		last.Err = fmt.Errorf("timed out after %s: %w", p.AttemptTimeout, last.Err)
		return true
	}
	return false
}

// mayRetry reports whether last can be retried; an auth failure must first recover.
func (p Policy) mayRetry(ctx context.Context, last *Error, attempt int) bool {
	if !last.Retryable() || attempt == p.MaxAttempts {
		return false
	}
	if last.Kind == KindAuth {
		if p.Recover == nil || p.Recover(ctx) != nil {
			return false
		}
	}
	return true
}

// backoffWait idles through wait, or until ctx ends.
func (p Policy) backoffWait(ctx context.Context, wait time.Duration) error {
	t := time.NewTimer(wait)
	ierr := p.idle(ctx, func() {
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	})
	t.Stop()
	return ierr
}

// TailBuffer keeps the last tailCap bytes of a child's stderr; the cause is at the end.
type TailBuffer struct{ buf []byte }

const tailCap = 8 << 10

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*tailCap {
		t.buf = t.buf[:copy(t.buf, t.buf[len(t.buf)-tailCap:])]
	}
	return len(p), nil
}

func (t *TailBuffer) String() string {
	if len(t.buf) > tailCap {
		return string(t.buf[len(t.buf)-tailCap:])
	}
	return string(t.buf)
}

// DefaultPerKey caps concurrent dispatches on one billed account (one credential, one rate limit).
const DefaultPerKey = 4

// sharedTryClaim is the machine-wide limiter seam; tests count or stub it.
var sharedTryClaim = inflight.TryClaim

// sharedPoll is how often a waiting dispatch re-checks the shared limiter; a var so tests can shorten it.
var sharedPoll = 250 * time.Millisecond

// dispatchSeq makes each permit's shared marker id unique within the process.
var dispatchSeq atomic.Uint64

// limiter is the one concurrency gate for dispatches: at most jobs run at once, and at most cap per billed account.
// A waiting task holds nothing, so a full account never blocks another.
type limiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	jobs, cap int
	per       map[string]int
	pending   int             // prepared tasks that have not yet reached their first dispatch: each still owes a slot
	limits    inflight.Limits // shared-limiter caps, read once per batch; zero Total = no shared limiter
}

// slot is one prepared task's claim on a global slot until its first dispatch asks for a permit (or parks, or ends).
type slot struct{ settled bool }

func newLimiter(jobs, cap int) *limiter {
	l := &limiter{jobs: jobs, cap: cap, per: map[string]int{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// wait blocks until ok() holds under the lock, or ctx is done; a cancelled ctx wins over a free slot.
func (l *limiter) wait(ctx context.Context, ok func() bool) error {
	defer context.AfterFunc(ctx, func() { l.mu.Lock(); l.cond.Broadcast(); l.mu.Unlock() })()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ok() {
			return nil
		}
		l.cond.Wait()
	}
}

// reserve waits, holding nothing, for a global slot no prepared task is owed, so a task is prepared only when it can
// start; the claim ends at settle.
func (l *limiter) reserve(ctx context.Context) (*slot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.wait(ctx, func() bool { return l.running+l.pending < l.jobs }); err != nil {
		return nil, err
	}
	l.pending++
	return &slot{}, nil
}

func (l *limiter) settleLocked(s *slot) {
	if s != nil && !s.settled {
		s.settled = true
		l.pending--
		l.cond.Broadcast()
	}
}

// settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func settle(ctx context.Context) {
	if l, _ := ctx.Value(limiterKey{}).(*limiter); l != nil {
		s, _ := ctx.Value(slotKey{}).(*slot)
		l.mu.Lock()
		l.settleLocked(s)
		l.mu.Unlock()
	}
}

// acquire waits for a global slot, an account permit and one shared slot; a full shared account releases the local
// permit before the next try, so it never holds a worker another account could use. The release func is idempotent.
func (l *limiter) acquire(ctx context.Context, account string) (func(), error) {
	if l.limits.Total <= 0 {
		return l.acquireLocal(ctx, account)
	}
	id := inflight.DispatchPrefix + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(dispatchSeq.Add(1), 10)
	c := inflight.Claim{Owner: os.Getpid(), ID: id, Account: account, N: 1}
	for {
		freeLocal, err := l.acquireLocal(ctx, account) // returns ctx.Err() first, so a cancelled dispatch claims nothing
		if err != nil {
			return nil, err
		}
		if ok, _ := sharedTryClaim(c, l.limits); ok {
			var once sync.Once
			return func() { once.Do(func() { inflight.Release(c.Owner, c.ID); freeLocal() }) }, nil
		}
		freeLocal()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sharedPoll):
		}
	}
}

// acquireLocal is the in-process admit: a global slot and, when account is set, an account permit.
func (l *limiter) acquireLocal(ctx context.Context, account string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, _ := ctx.Value(slotKey{}).(*slot)
	l.settleLocked(s) // a parked task no longer owes a slot; an admitted one takes it in this same critical section
	if err := l.wait(ctx, func() bool { return l.running < l.jobs && (account == "" || l.per[account] < l.cap) }); err != nil {
		return nil, err
	}
	l.running++
	if account != "" {
		l.per[account]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			if account != "" {
				l.per[account]--
			}
			l.cond.Broadcast()
		})
	}, nil
}

// permit is a dispatch's current permit; Do swaps it out around retry waits so a backing-off dispatch holds nothing.
type permit struct {
	account string
	release func()
	recheck func() *Error // a skip when the account became unusable since the dispatch started
}

// idle runs wait with the permit released, then takes a permit again; the dispatch runs a child only while holding one.
func (h *permit) idle(ctx context.Context, wait func()) error {
	h.release()
	wait()
	r, err := acquireDispatch(ctx, h.account)
	if err != nil {
		h.release = func() {}
		return err
	}
	h.release = r
	return nil
}

type (
	permitKey   struct{}
	limiterKey  struct{}
	failFastKey struct{}
	slotKey     struct{}
)

func withLimiter(ctx context.Context, l *limiter) context.Context {
	return context.WithValue(ctx, limiterKey{}, l)
}

// acquireDispatch takes a dispatch permit under ctx's limiter; outside a batch there is no limit.
func acquireDispatch(ctx context.Context, account string) (release func(), err error) {
	l, _ := ctx.Value(limiterKey{}).(*limiter)
	if l == nil {
		return func() {}, nil
	}
	return l.acquire(ctx, account)
}

// tripFailFast cancels the batch when err ends the task (no fallback follows), before the dispatch's permit is released.
func tripFailFast(ctx context.Context, c Chain, i int, req RunOptions, res Result, err error) {
	trip, _ := ctx.Value(failFastKey{}).(func())
	if trip == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if i+1 < len(c.Dispatches) && (i != 0 || !ExplicitBackend(req)) && dispatchUnavailable(c.Dispatches[i].Agent, res, err) {
		return
	}
	trip()
}

// Limiter is the batch runner's handle on the dispatch limiter; Slot is a prepared task's claim on a global slot.
type (
	Limiter = limiter
	Slot    = slot
)

// NewBatchLimiter is a limiter for a batch of jobs, with the shared limiter's caps read once.
func NewBatchLimiter(jobs int) *Limiter {
	l := newLimiter(jobs, DefaultPerKey)
	l.limits = router.CurrentLimits()
	return l
}

// Reserve waits for a global slot no prepared task is owed; the claim ends at Settle.
func (l *Limiter) Reserve(ctx context.Context) (*Slot, error) { return l.reserve(ctx) }

// WithBatchSlot carries l and s in ctx so Settle and the dispatches under it find them.
func WithBatchSlot(ctx context.Context, l *Limiter, s *Slot) context.Context {
	return context.WithValue(withLimiter(ctx, l), slotKey{}, s)
}

// WithBatchRun is ctx for one batch task's dispatches: host-isolated and under l.
func WithBatchRun(ctx context.Context, l *Limiter) context.Context {
	return withLimiter(withIsolation(ctx), l)
}

// WithFailFast makes a task that ends without a fallback call stop.
func WithFailFast(ctx context.Context, stop func()) context.Context {
	return context.WithValue(ctx, failFastKey{}, stop)
}

// Settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func Settle(ctx context.Context) { settle(ctx) }
