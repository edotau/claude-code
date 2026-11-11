package statusline

import (
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/usage"
)

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 15 * time.Minute

// limitsSegment is "7d:N%" on the first-party subscription session only: the payload's rate_limits, else
// internal/usage's cache file; no fresh data or a passed reset omits it. Any other session shows
// its credential's time left ("u2m:42min", "m2m:42min") when an expiry is recorded, else "".
func limitsSegment(in Input, session string) string {
	if !onSubscription(session) {
		return authSegment(session)
	}
	week := limitsFromPayload(in)
	if week < 0 {
		week = limitsFromCache()
	}
	if week < 0 {
		return ""
	}
	return fmt.Sprintf("7d:%d%%", week)
}

// authSegment is the session credential's countdown ("m2m:42m") from its expiry record; "" when none is recorded.
func authSegment(session string) string {
	exp, method, ok := providers.TokenExpiryMethod(session)
	if !ok {
		return ""
	}
	return tokenLeft(tokenLabel(session, method), exp, time.Now())
}

// onSubscription is true when the session profile row is first-party Anthropic on its cli (passthrough) login.
func onSubscription(session string) bool {
	reg := registry()
	if reg == nil {
		return false
	}
	p := reg.Providers[session]
	return p != nil && p.Provider == "anthropic" && p.Auth.Method() == providers.AuthCLI
}

// tokenLabel names the countdown's credential: the recorded mint method, else pat/m2m by the row's auth, else u2m.
func tokenLabel(session, recorded string) string {
	switch recorded {
	case "u2m", providers.AuthM2M, providers.AuthPAT:
		return recorded
	}
	if reg := registry(); reg != nil {
		if p := reg.Providers[session]; p != nil {
			if m := p.Auth.Method(); m == providers.AuthM2M || m == providers.AuthPAT {
				return m
			}
		}
	}
	return "u2m"
}

// tokenLeft renders the time until exp: "u2m:30m", "m2m:1h05m", "pat:89d", "u2m:<1m", or "u2m:exp" once past.
func tokenLeft(method string, exp, now time.Time) string {
	left := exp.Sub(now)
	switch {
	case left <= 0:
		return method + ":exp"
	case left < time.Minute:
		return method + ":<1m"
	case left < time.Hour:
		return fmt.Sprintf("%s:%dm", method, int(left/time.Minute))
	case left < 24*time.Hour:
		return fmt.Sprintf("%s:%dh%02dm", method, int(left/time.Hour), int(left%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%s:%dd", method, int(left/(24*time.Hour)))
}

// pct is the window's rounded used percent; -1 when absent or its reset already passed.
func (w *rateWindow) pct(now int64) int {
	if w == nil || w.UsedPercentage == nil || (w.ResetsAt != 0 && w.ResetsAt <= now) {
		return -1
	}
	return clampPct(int(*w.UsedPercentage + 0.5))
}

// limitsFromPayload reads the 7d window off the statusline payload; -1 means absent or expired.
func limitsFromPayload(in Input) int {
	if in.RateLimits == nil {
		return -1
	}
	return in.RateLimits.SevenDay.pct(time.Now().Unix())
}

// limitsFromCache reads internal/usage's cached 7d window (no network); -1 means absent, stale, expired or another login's.
func limitsFromCache() int {
	util, resets, ok := usage.CachedSevenDay(limitsCacheMaxAge)
	if !ok {
		return -1
	}
	if t, err := time.Parse(time.RFC3339, resets); err != nil || !t.After(time.Now()) {
		return -1 // the window's reset is unparsable or already passed
	}
	return clampPct(int(util + 0.5))
}
