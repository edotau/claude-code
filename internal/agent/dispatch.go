package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/providers"
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
	return !e.Progressed && (e.Kind == KindRateLimit || e.Kind == KindTransient || e.Kind == KindAuth)
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
		low := strings.ToLower(err.Error())
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			e.Kind = KindTransient
		case providers.IsQuota(low):
			e.Kind = KindQuota
		case rateRE.MatchString(low):
			e.Kind = KindRateLimit
		case authRE.MatchString(low):
			e.Kind = KindAuth
		case transientRE.MatchString(low):
			e.Kind = KindTransient
		}
		if m := retryAfterRE.FindStringSubmatch(low); m != nil {
			n, _ := strconv.Atoi(m[1])
			e.RetryAfter = time.Duration(n) * time.Second
			if m[2] != "" {
				e.RetryAfter = time.Duration(n) * time.Minute
			}
		}
	}
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) && ra.RetryAfter() > 0 {
		e.RetryAfter = ra.RetryAfter()
	}
	return e
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
			snap := *last
			snap.Earlier = nil
			earlier = append(earlier, &snap)
		}
		if cooling(p.Key) { // before the attempt's own timeout; the wait holds no permit, first attempt included
			if err := p.idle(ctx, func() { waitCooldown(ctx, p.Key, p.MaxDelay) }); err != nil {
				last = cmp.Or(last, &Error{Kind: KindFatal, Err: err})
				return fail()
			}
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if p.AttemptTimeout > 0 {
			actx, cancel = context.WithTimeout(ctx, p.AttemptTimeout)
		}
		last = fn(actx, attempt)
		attemptErr := actx.Err()
		cancel()
		if last == nil {
			return nil
		}
		last.Attempts = attempt
		if ctx.Err() != nil {
			last.Kind = KindFatal // the caller cancelled: never retry
			return fail()
		}
		// The attempt ran out its own AttemptTimeout (not the parent ctx): retrying would just
		// burn another full timeout for the same stuck call, so make it terminal and say so.
		if p.AttemptTimeout > 0 && errors.Is(attemptErr, context.DeadlineExceeded) {
			last.Kind = KindFatal
			last.Err = fmt.Errorf("timed out after %s: %w", p.AttemptTimeout, last.Err)
			return fail()
		}
		if !last.Retryable() || attempt == p.MaxAttempts {
			return fail()
		}
		if last.Kind == KindAuth {
			if p.Recover == nil || p.Recover(ctx) != nil {
				return fail()
			}
		}
		wait := p.Delay(last.RetryAfter, attempt, rand.Float64())
		if p.Key != "" && (last.Kind == KindRateLimit || last.Kind == KindTransient) {
			coolDown(p.Key, wait) // the other loops on this account back off too
		}
		if p.Notify != nil {
			p.Notify(fmt.Sprintf("%s: %s (attempt %d/%d), retrying in %s", last.Agent, last.Kind, attempt, p.MaxAttempts, wait.Round(100*time.Millisecond)))
		}
		t := time.NewTimer(wait)
		ierr := p.idle(ctx, func() {
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		})
		t.Stop()
		if ierr != nil || ctx.Err() != nil {
			return fail()
		}
	}
	return fail()
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
