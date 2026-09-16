package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is the failure class; it alone decides retryability.
type Kind int

const (
	KindNone      Kind = iota
	KindAuth           // credential rejected; retried only when a Recover can refresh it
	KindRateLimit      // 429 / quota
	KindTransient      // network, 5xx, overload, per-attempt timeout
	KindFatal          // everything else: bad request, missing binary, the agent's own verdict
)

func (k Kind) String() string {
	return [...]string{"ok", "auth rejected", "rate limited", "transient failure", "failed"}[k]
}

// Error is a classified runner failure.
type Error struct {
	Kind       Kind
	Agent      string
	Status     int // HTTP status when known
	RetryAfter time.Duration
	Progressed bool // output already reached the stream: never retried
	Attempts   int
	Err        error
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
	var done *Error
	if errors.As(err, &done) {
		done.Progressed = done.Progressed || progressed
		return done
	}
	e := &Error{Kind: KindFatal, Agent: agent, Progressed: progressed, Err: err}
	var st interface{ StatusCode() int }
	if errors.As(err, &st) {
		e.Status = st.StatusCode()
		e.Kind = KindForStatus(e.Status)
	} else {
		low := strings.ToLower(err.Error())
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			e.Kind = KindTransient
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
}

// DefaultPolicy suits slow, expensive agent runs: few attempts, real spacing.
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 3, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second, AttemptTimeout: 10 * time.Minute}
}

// Backoff is the equal-jitter wait after attempt (1-based); jitter in [0,1] keeps it pure for tests.
func (p Policy) Backoff(attempt int, jitter float64) time.Duration {
	window := float64(p.BaseDelay) * math.Pow(2, float64(max(attempt, 1)-1))
	if window > float64(p.MaxDelay) || math.IsInf(window, 0) {
		window = float64(p.MaxDelay)
	}
	return time.Duration(window/2 + jitter*window/2)
}

// Delay prefers the upstream's Retry-After, capped at MaxDelay.
func (p Policy) Delay(retryAfter time.Duration, attempt int, jitter float64) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, p.MaxDelay)
	}
	return p.Backoff(attempt, jitter)
}

// Do runs fn under the policy; fn gets a per-attempt context and the 1-based attempt number.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context, attempt int) *Error) *Error {
	var last *Error
	for attempt := 1; attempt <= max(p.MaxAttempts, 1); attempt++ {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if p.AttemptTimeout > 0 {
			actx, cancel = context.WithTimeout(ctx, p.AttemptTimeout)
		}
		last = fn(actx, attempt)
		cancel()
		if last == nil {
			return nil
		}
		last.Attempts = attempt
		if ctx.Err() != nil {
			last.Kind = KindFatal // the caller cancelled: never retry
			return last
		}
		if !last.Retryable() || attempt == p.MaxAttempts {
			return last
		}
		if last.Kind == KindAuth {
			if p.Recover == nil || p.Recover(ctx) != nil {
				return last
			}
		}
		wait := p.Delay(last.RetryAfter, attempt, rand.Float64())
		if p.Notify != nil {
			p.Notify(fmt.Sprintf("%s: %s (attempt %d/%d), retrying in %s", last.Agent, last.Kind, attempt, p.MaxAttempts, wait.Round(100*time.Millisecond)))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return last
		case <-t.C:
		}
	}
	return last
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
