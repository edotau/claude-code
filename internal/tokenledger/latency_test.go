package tokenledger

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSpendTotalsSessionAndDay(t *testing.T) {
	withLedgerDir(t)
	now := time.Now()
	Append(Entry{Ts: now, Session: "a", Model: "m", CostUSD: 1})
	Append(Entry{Ts: now, Session: "b", Model: "m", CostUSD: 2})
	Append(Entry{Ts: now, Model: "m", CostUSD: 4})
	Append(Entry{Ts: now.AddDate(0, 0, -1), Session: "a", Model: "m", CostUSD: 8})
	appendRaw(t, `{"schema":1,"ts":"`+now.Format(time.RFC3339Nano)+`","session":"a","model":"m","cost_usd":16}`)
	s, d, err := SpendTotals("a", now)
	if err != nil || s != 25 || d != 23 {
		t.Fatalf("a: session=%v day=%v err=%v, want 25 23", s, d, err)
	}
	if s, d, _ = SpendTotals("", now); s != 0 || d != 23 {
		t.Fatalf("empty session: session=%v day=%v, want 0 23", s, d)
	}
}

func TestLatencyStatsPercentiles(t *testing.T) {
	withLedgerDir(t)
	now := time.Now()
	Append(Entry{Ts: now.Add(-time.Hour), Provider: "p", Model: "m", FirstMs: 5, TotalMs: 5})
	Append(Entry{Ts: now, Provider: "p", Model: "m"})
	for i := int64(1); i <= 10; i++ {
		Append(Entry{Ts: now, Provider: "p", Profile: "r", Model: "m", FirstMs: i * 100, TotalMs: i * 200})
	}
	got, err := LatencyStats(now.Add(-time.Minute))
	l := got[LatencyKey{"p", "r", "m"}]
	if err != nil || len(got) != 1 || l.N != 10 || l.FirstP50 != 500 || l.FirstP95 != 1000 || l.TotalP50 != 1000 {
		t.Fatalf("got %+v err=%v", got, err)
	}
}

func TestLatencyStatsPartialFirstLine(t *testing.T) {
	withLedgerDir(t)
	now := time.Now()
	for i := int64(1); i <= 10; i++ {
		Append(Entry{Ts: now, Provider: "p", Profile: "r", Model: "m", FirstMs: i * 100, TotalMs: i * 200})
	}
	old := latencyTail
	t.Cleanup(func() { latencyTail = old })
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
	latencyTail = int64(len(strings.Join(lines[len(lines)-5:], "")) + 4)
	got, err := LatencyStats(now.Add(-time.Minute))
	l := got[LatencyKey{"p", "r", "m"}]
	if err != nil || l.N != 5 || l.FirstP50 != 800 || l.FirstP95 != 1000 {
		t.Fatalf("partial first line: %+v err=%v", got, err)
	}
}

func appendRaw(t *testing.T, s string) {
	t.Helper()
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
