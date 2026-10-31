// ledger.go reads internal/ledger's per-token entries and rolls them up for the API KEYS section.

package usage

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/ledger"
)

// ledgerAgg is one provider+profile+model row for the API KEYS section: tokens over today / 30d / all-time.
type ledgerAgg struct {
	Provider string `json:"provider"` // the platform
	Profile  string `json:"profile,omitempty"`
	Model    string `json:"model"`
	Today    int    `json:"today"`
	Last30d  int    `json:"last_30d"`
	AllTime  int    `json:"all_time"`
}

// aggregateLedger rolls entries up per provider+profile+model, sorted by provider, profile, then model.
func aggregateLedger(entries []ledger.Entry, now time.Time) []ledgerAgg {
	type key struct{ provider, profile, model string }
	byKey := map[key]*ledgerAgg{}
	var order []key
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()) // local midnight, not UTC
	cutoff30 := now.Add(-30 * 24 * time.Hour)
	for _, e := range entries {
		k := key{e.Provider, e.ProfileName(), e.Model}
		a, ok := byKey[k]
		if !ok {
			a = &ledgerAgg{Provider: e.Provider, Profile: k.profile, Model: e.Model}
			byKey[k] = a
			order = append(order, k)
		}
		total := e.Input + e.Output + e.CacheRead
		a.AllTime += total
		if !e.Ts.Before(cutoff30) {
			a.Last30d += total
		}
		if !e.Ts.Before(today) {
			a.Today += total
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.profile, b.profile), cmp.Compare(a.model, b.model))
	})
	out := make([]ledgerAgg, len(order))
	for i, k := range order {
		out[i] = *byKey[k]
	}
	return out
}

// ledgerSince is the earliest entry's timestamp, used for the "ledger started <date>" empty message.
func ledgerSince(entries []ledger.Entry) time.Time {
	var since time.Time
	for _, e := range entries {
		if since.IsZero() || e.Ts.Before(since) {
			since = e.Ts
		}
	}
	return since
}

// humanTokens renders a token count compactly: 1.2M, 34k, 512.
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

// rowLabel is "<provider>·<profile>", the provider alone when the profile is empty or bears its name.
func rowLabel(provider, profile string) string {
	if provider == "" {
		return profile
	}
	if profile == "" || profile == provider {
		return provider
	}
	return provider + "·" + profile
}
