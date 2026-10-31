// routestate.go — feeds internal/route's quota.json: every fetched account's tightest window, and a targeted
// one-account refresh a pick runs when its entry is stale.

package usage

import (
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// recordQuotaState stores the fetched rows in quota.json; a var so package tests never write the real file.
var recordQuotaState = func(qs []quota) { recordQuotas(qs, time.Now()) }

// recordQuotas keeps each account's worst window (Fable's model cap and failure rows excluded); returns how many it stored.
func recordQuotas(qs []quota, now time.Time) int {
	worst := map[string]quota{}
	for _, q := range qs {
		key := accountKey(q)
		if key == "" || q.Window == "" || q.Group != "" || (q.Note != "" && !strings.HasPrefix(q.Note, "cached")) {
			continue
		}
		if w, ok := worst[key]; !ok || q.UsedPct > w.UsedPct {
			worst[key] = q
		}
	}
	n := 0
	for key, q := range worst {
		if router.Record(key, q.UsedPct, q.Window, resetTime(q), now) == nil {
			n++
		}
	}
	return n
}

// accountKey maps a quota row to route's account key; "" for providers the router has no column for.
func accountKey(q quota) string {
	switch q.Provider {
	case "anthropic":
		return "anthropic·max"
	case "openai":
		return "openai·chatgpt"
	case "github":
		if q.Account != "" {
			reg, _ := providers.Load()
			return "github·" + providers.GithubAccountName(reg, q.Account)
		}
	}
	return ""
}

// resetTime reads a row's reset: the UTC instant when set, else the local display forms.
func resetTime(q quota) time.Time {
	if t, err := time.Parse(time.RFC3339, q.ResetsAt); err == nil {
		return t
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, q.Resets, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// RefreshAccount re-fetches one account (3 s; Anthropic's 60 s cache reused) and records it; false on no data.
func RefreshAccount(ctx context.Context, key string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var qs []quota
	switch {
	case key == "anthropic·max":
		qs = claudeQuotas(ctx)
	case key == "openai·chatgpt":
		qs = openaiQuotas(ctx)
	case strings.HasPrefix(key, "github·"):
		reg, _ := providers.Load()
		qs = []quota{githubQuota(ctx, providers.GithubAccountLogin(reg, strings.TrimPrefix(key, "github·")))}
	default:
		return false
	}
	setStates(qs, warnThreshold())
	return recordQuotas(qs, time.Now()) > 0
}

// ClaudePressure is the Anthropic plan's tightest window (5h or weekly; Fable excluded) from the shared usage
// cache when fetched within router.StaleAfter; never touches the network, so a hook may call it.
func ClaudePressure(now time.Time) (pct float64, window string, ok bool) {
	c := readClaudeUsageCache(claudeUsageCachePath())
	if c == nil || now.Sub(c.FetchedAt) > router.StaleAfter {
		return 0, "", false
	}
	for _, q := range parseClaudeUsage(c.Body, c.SubscriptionType) {
		if q.Group == "" && q.Window != "" && q.UsedPct >= pct {
			pct, window, ok = q.UsedPct, q.Window, true
		}
	}
	return pct, window, ok
}
