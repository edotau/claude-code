// Package retry is retry, backoff, cooldown and error classification plus the dispatch limiter for agent dispatches.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
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
	Earlier    []*Error      // the attempts before this one, oldest first; Do sets it on the error it returns
	Terminal   bool          // a state check refused the attempt: retrying cannot help
	Waited     time.Duration // wall time Do spent on attempts and waits before giving up
	Connect    bool          // failed before any response (dial, TLS, tunnel), not an HTTP status: Do tries the other egress path
}

// Fatal is a terminal, never-retried failure of agent wrapping err.
func Fatal(agent string, err error) error {
	return &Error{Kind: KindFatal, Agent: agent, Err: err}
}

// Progressed is true when err is an *Error whose output already reached the stream.
func Progressed(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Progressed
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Agent, e.Kind)
	if inner, ok := e.Err.(*Error); ok && inner.Agent == e.Agent && inner.Kind == e.Kind {
		msg = inner.Error() // an engine already wrapped its own failure; wrapping it again repeats "<agent>: <kind>:"
	} else if e.Err != nil {
		msg += ": " + strings.TrimPrefix(e.Err.Error(), e.Agent+": ") // an engine error may lead with its own agent name
	}
	if e.Kind == KindRateLimit && e.Attempts > 0 && e.Waited > 0 {
		msg += fmt.Sprintf(" (gave up after %d %s over %s; requeue this dispatch later, do not re-ask now)", e.Attempts, textutil.PluralWord(e.Attempts, "try", "tries"), roundWaited(e.Waited))
	} else if e.Attempts > 1 {
		msg += fmt.Sprintf(" (after %d attempts)", e.Attempts)
	}
	return msg
}

// roundWaited rounds to whole seconds, or to milliseconds under one second.
func roundWaited(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

func (e *Error) Unwrap() error { return e.Err }

// AttemptMessage is Error() as one attempt saw it, without the "(after N attempts)" tally.
func (e *Error) AttemptMessage() string {
	one := *e
	one.Attempts = 0
	return one.Error()
}

// retryable is true for rate-limit and transient failures that have not streamed output.
func (e *Error) retryable() bool {
	return !e.Progressed && !e.Terminal && (e.Kind == KindRateLimit || e.Kind == KindTransient || e.Kind == KindAuth)
}

// kindForStatus maps an HTTP status to a Kind; 0 means the transport never completed.
func kindForStatus(status int) Kind {
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
	connectRE    = regexp.MustCompile(`dial tcp|\btls\b|i/o timeout|connection refused|no such host|egress host`)
	retryAfterRE = regexp.MustCompile(`(?:retry[- ]after|try again in)[:\s]+(\d+)\s*(?:(h|hrs?|hours?|m|min|minutes?|ms|s|seconds?)(?:\b|\d))?`)
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
		e.Kind = kindForStatus(e.Status)
		if e.Kind == KindRateLimit && providers.IsQuota(err.Error()) {
			e.Kind = KindQuota
		}
	} else {
		e.Kind, e.RetryAfter = classifyText(err)
		e.Connect = preResponseConnect(e.Kind, err)
	}
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) && ra.RetryAfter() > 0 {
		e.RetryAfter = ra.RetryAfter()
	}
	return e
}

// preResponseConnect is true when a transient or fatal text-classified error is a connection failure.
func preResponseConnect(kind Kind, err error) bool {
	return (kind == KindTransient || kind == KindFatal) && connectFailure(err)
}

// connectFailure is true for a dial, TLS or network timeout (a deadline of the attempt itself is not one).
func connectFailure(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
		return true
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() && !errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return connectRE.MatchString(strings.ToLower(err.Error()))
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
	return kind, retryAfterText(low)
}

// maxHintUnits keeps n * time.Hour well inside int64 nanoseconds.
const maxHintUnits = 1_000_000

// retryAfterText reads a "retry after N ms|seconds|minutes" hint out of lower-cased error text.
func retryAfterText(low string) time.Duration {
	m := retryAfterRE.FindStringSubmatch(low)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n > maxHintUnits {
		n = maxHintUnits // overflow or absurd: clamp so the duration stays positive and MaxDelay caps it
	}
	switch m[2] {
	case "h", "hr", "hrs", "hour", "hours":
		return time.Duration(n) * time.Hour
	case "ms":
		return time.Duration(n) * time.Millisecond
	case "m", "min", "minute", "minutes":
		return time.Duration(n) * time.Minute
	}
	return time.Duration(n) * time.Second
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

// Default backoff bounds and the no-hint rate-limit floor are variables so tests can shrink them.
var (
	DefaultBaseDelay = 2 * time.Second
	DefaultMaxDelay  = 60 * time.Second
	RateLimitFloor   = 30 * time.Second
)

// DefaultPolicy suits slow, expensive agent runs: few attempts, real spacing.
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 3, BaseDelay: DefaultBaseDelay, MaxDelay: DefaultMaxDelay, AttemptTimeout: 10 * time.Minute}
}

// backoff is the equal-jitter wait after attempt (1-based); jitter in [0,1] keeps it pure for tests.
func (p Policy) backoff(attempt int, jitter float64) time.Duration {
	window := float64(p.BaseDelay) * math.Pow(2, float64(max(attempt, 1)-1))
	if window > float64(p.MaxDelay) || math.IsInf(window, 0) {
		window = float64(p.MaxDelay)
	}
	return time.Duration(window/2 + jitter*window/2)
}

// delay prefers the upstream's Retry-After plus jitter (so tasks given the same one wake apart), capped at MaxDelay.
func (p Policy) delay(retryAfter time.Duration, attempt int, jitter float64) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter+time.Duration(jitter*float64(min(retryAfter/4, p.BaseDelay))), p.MaxDelay)
	}
	return p.backoff(attempt, jitter)
}

// cooldowns is when each provider key may be tried again, shared by every Do in the process.
var cooldowns = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// CoolDown holds key back for d; a shorter hold never replaces a longer one.
func CoolDown(key string, d time.Duration) {
	cooldowns.Lock()
	defer cooldowns.Unlock()
	if t := time.Now().Add(d); t.After(cooldowns.until[key]) {
		cooldowns.until[key] = t
	}
}

// ResetCooldowns drops every hold; tests call it so one test's 429 never delays the next test's dispatch.
func ResetCooldowns() {
	cooldowns.Lock()
	defer cooldowns.Unlock()
	clear(cooldowns.until)
}

// WaitCooldown blocks until key's cooldown ends, for at most maxWait (0 returns at once), or until ctx is done.
func WaitCooldown(ctx context.Context, key string, maxWait time.Duration) {
	if key == "" {
		return
	}
	limit := time.Now().Add(maxWait)
	for {
		cooldowns.Lock()
		wait := time.Until(cooldowns.until[key]) // re-read: another task may have extended the hold while we slept
		cooldowns.Unlock()
		wait = min(wait, time.Until(limit))
		if wait <= 0 {
			return
		}
		Settle(ctx) // a cooling task holds no slot, so a task on another account can start
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
	fn = p.reroute(fn, &earlier)
	start := time.Now()
	fail := func() *Error { // every attempt before the last rides on the error returned
		last.Earlier = earlier
		last.Waited = time.Since(start)
		return last
	}
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if last != nil {
			earlier = append(earlier, WithoutEarlier(last))
		}
		if cooling(p.Key) { // before the attempt's own timeout; the wait holds no permit, first attempt included
			if err := p.idle(ctx, func() { WaitCooldown(ctx, p.Key, p.MaxDelay) }); err != nil {
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
		if p.terminal(ctx, last, nil) { // cancelled during the wait: fatal, like a cancel during the attempt
			return fail()
		}
	}
	return fail()
}

// reroute wraps fn so the first connection failure is retried at once over the other egress path, never again.
func (p Policy) reroute(fn func(context.Context, int) *Error, earlier *[]*Error) func(context.Context, int) *Error {
	tried := false
	return func(ctx context.Context, attempt int) *Error {
		e := fn(ctx, attempt)
		if e == nil || !e.Connect || tried || ctx.Err() != nil {
			return e
		}
		alt, from, to, ok := egress.Alternate(ctx)
		if !ok {
			return e
		}
		tried = true
		if p.Notify != nil {
			p.Notify(fmt.Sprintf("%s: unreachable via %s, trying %s", e.Agent, from, to))
		}
		e.Attempts = attempt
		*earlier = append(*earlier, WithoutEarlier(e))
		return fn(alt, attempt)
	}
}

func (p Policy) notifyRetry(last *Error, attempt int, wait time.Duration) {
	if p.Notify != nil {
		p.Notify(fmt.Sprintf("%s: %s (attempt %d/%d), retrying in %s", last.Agent, last.Kind, attempt, p.MaxAttempts, wait.Round(100*time.Millisecond)))
	}
}

func (p Policy) retryDelay(last *Error, attempt int) time.Duration {
	jitter := rand.Float64()
	wait := p.delay(last.RetryAfter, attempt, jitter)
	if last.Kind == KindRateLimit && last.RetryAfter <= 0 {
		wait = max(wait, RateLimitFloor+time.Duration(jitter*float64(RateLimitFloor)/2))
	}
	if p.Key != "" && (last.Kind == KindRateLimit || last.Kind == KindTransient) {
		CoolDown(p.Key, wait) // Other loops back off too, even when this one gives up.
	}
	return wait
}

// WithoutEarlier is a copy of e with its own Earlier chain cut.
func WithoutEarlier(e *Error) *Error {
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
			e.Earlier = append(slices.Clone(earlier), WithoutEarlier(last))
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
	// The attempt hit its own AttemptTimeout, not the parent ctx: retrying would burn another full timeout, so it ends.
	if p.AttemptTimeout > 0 && errors.Is(attemptErr, context.DeadlineExceeded) {
		last.Kind = KindFatal
		last.Err = fmt.Errorf("timed out after %s: %w", p.AttemptTimeout, last.Err)
		return true
	}
	return false
}

// mayRetry reports whether last can be retried; an auth failure must first recover.
func (p Policy) mayRetry(ctx context.Context, last *Error, attempt int) bool {
	if !last.retryable() || attempt == p.MaxAttempts {
		return false
	}
	if last.Kind == KindAuth {
		if p.Recover == nil {
			return false
		}
		if rerr := p.Recover(ctx); rerr != nil {
			last.Err = fmt.Errorf("%w (credential refresh failed: %w)", last.Err, rerr)
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
