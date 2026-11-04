package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 6 * time.Hour

// limitsSegment is the session's auth cell: "7d:N%" on the first-party subscription, else the credential's time left.
func limitsSegment(in Input, session string) string {
	p := rowOf(session)
	return labelerFor(p).auth(in, session, p)
}

// tokenAuth is the credential's time left ("u2m:42min", "m2m:42min") when an expiry is recorded, else "".
func tokenAuth(_ Input, session string, p *providers.Provider) string {
	exp, ok := providers.TokenExpiry(session)
	if !ok {
		return ""
	}
	method := "u2m" // a recorded expiry is a user OAuth JWT unless the row is a service principal
	if p != nil && p.Auth.Method() == providers.AuthM2M {
		method = providers.AuthM2M
	}
	return tokenLeft(method, exp, time.Now())
}

// tokenLeft renders the time until exp: "u2m:42min", "m2m:1h05min", "u2m:<1min", or "u2m:exp" once past.
func tokenLeft(method string, exp, now time.Time) string {
	left := exp.Sub(now)
	switch {
	case left <= 0:
		return method + ":exp"
	case left < time.Minute:
		return method + ":<1min"
	case left < time.Hour:
		return fmt.Sprintf("%s:%dmin", method, int(left/time.Minute))
	}
	return fmt.Sprintf("%s:%dh%02dmin", method, int(left/time.Hour), int(left%time.Hour/time.Minute))
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

// limitsFromCache reads the 7d window from the usage cache internal/usage writes (no network); -1 means absent, stale or expired.
func limitsFromCache() int {
	raw, err := os.ReadFile(filepath.Join(paths.StateDir(), "cache", "claude-oauth-usage.json"))
	if err != nil {
		return -1
	}
	var c struct {
		FetchedAt time.Time       `json:"fetched_at"`
		Body      json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &c) != nil || len(c.Body) == 0 || time.Since(c.FetchedAt) > limitsCacheMaxAge {
		return -1
	}
	var b struct {
		SevenDay cacheWindow `json:"seven_day"`
	}
	if json.Unmarshal(c.Body, &b) != nil {
		return -1
	}
	return b.SevenDay.pct(time.Now())
}
