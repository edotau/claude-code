package usage

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/dev/tokenledger"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func TestCostEstimate(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	entries := []tokenledger.Entry{
		{Ts: day, Profile: "dev", Model: "m1", Input: 1_000_000, Output: 500_000, CacheRead: 2_000_000, CacheWrite: 300_000, CacheWrite1h: 100_000},
		{Ts: day.Add(time.Hour), Profile: "dev", Model: "Dev:M1[1m]", Input: 500_000},
		{Ts: day.Add(time.Hour), Profile: "dev", Model: "databricks-m1", Input: 500_000},
		{Ts: day, Profile: "other", Model: "m1", Input: 5_000_000}, // a profile with no price table is not listed
		{Ts: day, Profile: "dev", Model: "free", Input: 7},
		{Ts: day.Add(-48 * time.Hour), Profile: "dev", Model: "m1", Input: 9_000_000},
	}
	prices := map[string]map[string]providers.Price{"dev": {"M1": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}}
	tests := []struct {
		name    string
		model   string
		in      int
		wantUSD float64
	}{
		{"priced", "m1", 2_000_000, 2*3 + 0.5*15 + 2*0.3 + 0.3*3.75},
		{"unpriced", "free", 7, math.NaN()},
	}
	rows := estimateCost(entries, prices, day.Add(-time.Hour))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := slices.IndexFunc(rows, func(r costRow) bool { return r.Model == tt.model })
			if i < 0 || rows[i].Input != tt.in || rows[i].Source != "est." || rows[i].Profile != "dev" {
				t.Fatalf("rows = %+v", rows)
			}
			if !sameUSD(rows[i].USD, tt.wantUSD) {
				t.Errorf("USD = %v, want %v", rows[i].USD, tt.wantUSD)
			}
		})
	}
}

func TestCostCacheWrite1hRate(t *testing.T) {
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	entries := []tokenledger.Entry{{Ts: day, Profile: "dev", Model: "m1", CacheWrite: 300_000, CacheWrite1h: 100_000}}
	prices := map[string]map[string]providers.Price{"dev": {"m1": {CacheWrite: 3.75, CacheWrite1h: 6}}}
	rows := estimateCost(entries, prices, day)
	if want := 0.2*3.75 + 0.1*6; len(rows) != 1 || !sameUSD(rows[0].USD, want) {
		t.Errorf("rows = %+v, want USD %v", rows, want)
	}
}

// sameUSD treats two NaNs (an unpriced model) as equal.
func sameUSD(got, want float64) bool {
	return (math.IsNaN(got) && math.IsNaN(want)) || math.Abs(got-want) < 1e-9
}

func TestCostPrintSection(t *testing.T) {
	stubCost(t, nil, costReg(nil))
	rows := []costRow{
		{Profile: "dev", Model: "a", Input: 1_500_000, USD: 1.25, Source: "est."},
		{Profile: "dev", Model: "b", Input: 10, USD: math.NaN(), Source: "est."},
	}
	var sb strings.Builder
	printCostSection(&sb, rows, "last 30 days")
	out := sb.String()
	for _, want := range []string{"(est.)", "1.5M", "$1.25", "total (1 unpriced)", "COST, last 30 days (est.)", "totals leave out unpriced models",
		"est.: local ledger tokens x your price table, not an invoice"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCacheHitCell(t *testing.T) {
	for _, tt := range []struct {
		r    costRow
		want string
	}{
		{costRow{Input: 10, CacheRead: 80, CacheWrite: 10}, "80%"},
		{costRow{CacheRead: 5}, "100%?"},
		{costRow{Output: 3}, "-"},
		{costRow{Input: 5, CacheWrite: 5}, "0%"},
	} {
		if got := cacheHitCell(tt.r); got != tt.want {
			t.Errorf("cacheHitCell(%+v) = %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestCostWidth(t *testing.T) {
	stubCost(t, nil, costReg(nil))
	long := strings.Repeat("databricks-claude-sonnet-4-5-", 4)
	rows := []costRow{
		{Profile: strings.Repeat("p", 40), Model: long, Input: 123_456_789, Output: 98_765_432, CacheRead: 1, CacheWrite: 2, USD: 12345.67, Source: "est."},
	}
	var sb strings.Builder
	printCostSection(&sb, rows, "last 7 days")
	for line := range strings.SplitSeq(sb.String(), "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line is %d columns: %q", n, line)
		}
	}
}

func TestCostBudgetLine(t *testing.T) {
	b := Budget{Profile: "dev", Number: 1, Scope: "ALERT_CONFIGURATION_SCOPE_TYPE_PER_USER", Period: "MONTH", Limit: 100}
	for _, spend := range []float64{79, 80, 125} {
		b.Spend = spend
		got := costBudgetLine(b)
		if strings.Contains(got, "WARNING") != (spend >= 80) {
			t.Errorf("spend %v: wrong warning: %s", spend, got)
		}
		for _, want := range []string{"budget est.: dev #1 per-user/month", "spend / $100.00 limit"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q: %s", want, got)
			}
		}
	}
}

// An owner price wins; the list price fills only a model the owner left unpriced; no price stays NaN.
func TestCostRowPricedOwnerBeforeList(t *testing.T) {
	list := func(_, id string) (providers.Price, bool) {
		return providers.Price{Input: 2, Output: 2}, id != "unknown"
	}
	owner := map[string]providers.Price{"owned": {Input: 1, Output: 1}}
	for _, c := range []struct {
		model    string
		usd      float64
		fromList bool
	}{{"owned", 1, false}, {"listed", 2, true}, {"unknown", math.NaN(), false}} {
		got := costRow{Model: c.model, Input: 1_000_000, Source: costEst}.priced(owner, list)
		if got.List != c.fromList || (got.USD != c.usd && !(math.IsNaN(got.USD) && math.IsNaN(c.usd))) {
			t.Errorf("%s = USD %v list %v, want %v %v", c.model, got.USD, got.List, c.usd, c.fromList)
		}
	}
	if (costRow{Source: costEst, List: true}).label() != "est. list" {
		t.Error("a list-priced row must be labelled est. list")
	}
}
