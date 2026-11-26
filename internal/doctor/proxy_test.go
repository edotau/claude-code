package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func TestScopeOverReserved(t *testing.T) {
	cfg := proxyteam.Config{Teams: []proxyteam.Team{
		{Name: "d", Host: proxyteam.HostDetached, Reserved: 2},
		{Name: "a", Host: proxyteam.HostAsk, Reserved: 5},
	}}
	for _, c := range []struct {
		team, id string
		reserved int
		over     bool
	}{
		{"d", "0", 2, false}, {"d", "1", 2, false}, {"d", "2", 2, true},
		{"a", "0", 0, true}, {"gone", "0", 0, true},
	} {
		n, over := scopeOverReserved(proxy.Scope{Team: c.team, ID: c.id}, cfg)
		if n != c.reserved || over != c.over {
			t.Errorf("%s@%s = (%d, %v), want (%d, %v)", c.team, c.id, n, over, c.reserved, c.over)
		}
	}
}

func TestSpendCapStatus(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	ok := func(v float64) func() (float64, error) { return func() (float64, error) { return v, nil } }
	boom := func() (float64, error) { return 0, errors.New("boom") }
	never := func() (float64, error) { t.Fatal("spent called while off"); return 0, nil }
	for _, c := range []struct {
		name  string
		cap   providers.SpendCap
		spent func() (float64, error)
		want  string
		bad   bool
	}{
		{"off", providers.SpendCap{}, never, "off (claude-code configure routing spendCap.dayUSD=<usd>)", false},
		{"under", providers.SpendCap{DayUSD: f(50), SessionUSD: f(5)}, ok(1.5), "today $1.50 of $50.00 day cap; session cap $5.00", false},
		{"over", providers.SpendCap{DayUSD: f(50)}, ok(50), "today $50.00 of $50.00 day cap", true},
		{"session only", providers.SpendCap{SessionUSD: f(5)}, ok(2), "day cap off; session cap $5.00", false},
		{"read error", providers.SpendCap{DayUSD: f(50)}, boom, "ledger unreadable: boom", true},
	} {
		got, bad := spendCapStatus(c.cap, c.spent)
		if !strings.Contains(got, c.want) || bad != c.bad {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, bad, c.want, c.bad)
		}
	}
}
