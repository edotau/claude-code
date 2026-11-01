package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 6 * time.Hour

// limitsSegment is "5h:N% 7d:N%" on the first-party subscription session only: the payload's rate_limits, else
// internal/usage's cache file; a window with no fresh data or a passed reset is omitted. Any other session shows
// its credential's time left ("oauth:42m", "tok:42m") when an expiry is recorded, else "".
func limitsSegment(in Input, session string) string {
	if !onSubscription(session) {
		exp, ok := providers.TokenExpiry(session)
		if !ok {
			return ""
		}
		return tokenLeft(tokenLabel(session), exp, time.Now())
	}
	five, week := limitsFromPayload(in)
	if five < 0 && week < 0 {
		five, week = limitsFromCache()
	}
	var parts []string
	if five >= 0 {
		parts = append(parts, fmt.Sprintf("5h:%d%%", five))
	}
	if week >= 0 {
		parts = append(parts, fmt.Sprintf("7d:%d%%", week))
	}
	return strings.Join(parts, " ")
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

// tokenLabel names the credential in the countdown: "m2m" for a service principal, else "oauth" (a recorded expiry is an OAuth JWT).
func tokenLabel(session string) string {
	if reg := registry(); reg != nil {
		if p := reg.Providers[session]; p != nil {
			if p.Auth.Method() == providers.AuthM2M {
				return providers.AuthM2M
			}
		}
	}
	return providers.AuthOAuth
}

// tokenLeft renders the time until exp: "oauth:42m", "tok:1h05m", "oauth:<1m", or "oauth:exp" once past.
func tokenLeft(method string, exp, now time.Time) string {
	left := exp.Sub(now)
	switch {
	case left <= 0:
		return method + ":exp"
	case left < time.Minute:
		return method + ":<1m"
	case left < time.Hour:
		return fmt.Sprintf("%s:%dm", method, int(left/time.Minute))
	}
	return fmt.Sprintf("%s:%dh%02dm", method, int(left/time.Hour), int(left%time.Hour/time.Minute))
}

// pct is the window's rounded used percent; -1 when absent or its reset already passed.
func (w *rateWindow) pct(now int64) int {
	if w == nil || w.UsedPercentage == nil || (w.ResetsAt != 0 && w.ResetsAt <= now) {
		return -1
	}
	return clampPct(int(*w.UsedPercentage + 0.5))
}

// limitsFromPayload reads rate_limits off the statusline payload; -1 means absent or expired.
func limitsFromPayload(in Input) (five, week int) {
	if in.RateLimits == nil {
		return -1, -1
	}
	now := time.Now().Unix()
	return in.RateLimits.FiveHour.pct(now), in.RateLimits.SevenDay.pct(now)
}

// cacheWindow is one window of internal/usage's oauth/usage cache body.
type cacheWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// pct is the window's rounded utilization; -1 when its reset is unparsable or already passed.
func (w cacheWindow) pct(now time.Time) int {
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err != nil || !t.After(now) {
		return -1
	}
	return clampPct(int(w.Utilization + 0.5))
}

// limitsFromCache reads the usage cache internal/usage writes (no network); -1 means absent, stale or expired.
func limitsFromCache() (five, week int) {
	raw, err := os.ReadFile(filepath.Join(paths.StateDir(), "cache", "claude-oauth-usage.json"))
	if err != nil {
		return -1, -1
	}
	var c struct {
		FetchedAt time.Time       `json:"fetched_at"`
		Body      json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &c) != nil || len(c.Body) == 0 || time.Since(c.FetchedAt) > limitsCacheMaxAge {
		return -1, -1
	}
	var b struct {
		FiveHour cacheWindow `json:"five_hour"`
		SevenDay cacheWindow `json:"seven_day"`
	}
	if json.Unmarshal(c.Body, &b) != nil {
		return -1, -1
	}
	now := time.Now()
	return b.FiveHour.pct(now), b.SevenDay.pct(now)
}
