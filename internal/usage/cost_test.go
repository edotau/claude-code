package usage

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/databricks/sqlexec"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tokenledger"
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

func TestCostBilledRows(t *testing.T) {
	through := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		cols    []string
		rows    [][]string
		wantErr string
		usd     float64
	}{
		{"by name, reordered", []string{"usd", "through", "model", "profile", "dbus"},
			[][]string{{"12.5", "2026-11-18", "m1", "dev", "40"}}, "", 12.5},
		{"missing column", []string{"profile", "model", "dbus", "usd"},
			[][]string{{"dev", "m1", "1", "2"}}, `"through"`, 0},
		{"bad number", []string{"profile", "model", "dbus", "usd", "through"},
			[][]string{{"dev", "m1", "x", "2", "2026-11-18"}}, "dbus", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := billedRows(tt.cols, tt.rows)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			want := costRow{Profile: "dev", Model: "m1", DBUs: 40, USD: tt.usd, Source: "billed", Through: through}
			if err != nil || len(got) != 1 || got[0] != want {
				t.Fatalf("got %+v, err %v, want %+v", got, err, want)
			}
		})
	}
}

func TestCostBilledNullIsUnknown(t *testing.T) {
	cols := []string{"profile", "model", "dbus", "usd", "through"}
	got, err := billedRows(cols, [][]string{{"dev", "m1", sqlexec.NullCell, sqlexec.NullCell, sqlexec.NullCell}, {"dev", "m2", "", "0", "2026-11-18"}})
	if err != nil || len(got) != 2 || !math.IsNaN(got[0].USD) || !math.IsNaN(got[0].DBUs) || !math.IsNaN(got[1].DBUs) || got[1].USD != 0 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if !got[0].Through.IsZero() {
		t.Errorf("null through = %v, want zero", got[0].Through)
	}
}

func TestCostThroughFormats(t *testing.T) {
	want := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"2026-11-18", "2026-11-18T00:00:00Z", "2026-11-18 00:00:00"} {
		if got, err := parseThrough(in); err != nil || !got.Equal(want) {
			t.Errorf("parseThrough(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"18/11/2026", "2026-11-18junk", "yesterday"} {
		if _, err := parseThrough(in); err == nil || !strings.Contains(err.Error(), "through") {
			t.Errorf("parseThrough(%q) err = %v, want a clear error", in, err)
		}
	}
}

func TestCostPrintSection(t *testing.T) {
	stubCost(t, nil, costReg(nil))
	rows := []costRow{
		{Profile: "dev", Model: "a", Input: 1_500_000, USD: 1.25, Source: "est."},
		{Profile: "dev", Model: "b", Input: 10, USD: math.NaN(), Source: "est."},
		{Profile: "dev", Model: "a", DBUs: 3, USD: 2, Source: "billed", Through: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)},
	}
	var sb strings.Builder
	printCostSection(&sb, rows, "last 30 days")
	out := sb.String()
	for _, want := range []string{"(est.)", "(billed)", "1.5M", "$1.25", "total (partial: 1 unpriced)", "COST, last 30 days (est.)", "totals leave out unpriced models",
		"est.: local ledger tokens x your price table, not an invoice",
		"billed: system.billing, through 2026-11-18"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCostWidth(t *testing.T) {
	stubCost(t, nil, costReg(nil))
	long := strings.Repeat("databricks-claude-sonnet-4-5-", 4)
	rows := []costRow{
		{Profile: strings.Repeat("p", 40), Model: long, Input: 123_456_789, Output: 98_765_432, CacheRead: 1, CacheWrite: 2, USD: 12345.67, Source: "est."},
		{Profile: "dev", Model: long, DBUs: 1234.5, USD: 99999.99, Source: "billed", Through: time.Now()},
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
