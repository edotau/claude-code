package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 6 * time.Hour

// limitsSegment is "5h:N% 7d:N%": the statusline payload's rate_limits, else internal/usage's cache file.
// A window with no fresh data, or whose reset already passed, is omitted; "" when both are.
func limitsSegment(in Input) string {
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
