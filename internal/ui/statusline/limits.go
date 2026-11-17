package statusline

import (
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/usage"
)

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 15 * time.Minute

// limitsSegment is "7d:N%" on the first-party subscription session only; no fresh data or a passed reset omits it.
func limitsSegment(in input) string {
	week := limitsFromPayload(in)
	if week < 0 {
		week = limitsFromCache()
	}
	if week < 0 {
		return ""
	}
	return fmt.Sprintf("7d:%d%%", week)
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

// pct is the window's rounded used percent; -1 when absent or its reset already passed.
func (w *rateWindow) pct(now int64) int {
	if w == nil || w.UsedPercentage == nil || (w.ResetsAt != 0 && w.ResetsAt <= now) {
		return -1
	}
	return clampPct(int(*w.UsedPercentage + 0.5))
}

// limitsFromPayload reads the 7d window off the statusline payload; -1 means absent or expired.
func limitsFromPayload(in input) int {
	if in.RateLimits == nil {
		return -1
	}
	return in.RateLimits.SevenDay.pct(time.Now().Unix())
}

// limitsFromCache returns internal/usage's cached 7d window without network; -1 if absent, stale, expired or foreign.
func limitsFromCache() int {
	util, resets, ok := usage.CachedSevenDay(limitsCacheMaxAge)
	if !ok {
		return -1
	}
	if t, err := time.Parse(time.RFC3339, resets); err != nil || !t.After(time.Now()) {
		return -1
	}
	return clampPct(int(util + 0.5))
}
