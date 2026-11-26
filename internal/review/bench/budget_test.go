package bench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestAllowCountLimit(t *testing.T) {
	for _, kind := range []string{KindBenchLoop, KindGateFix} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			now := time.Now()
			for i := range budgetLimit(kind) {
				if ok, why := allow(kind, now); !ok {
					t.Fatalf("refused before limit: %s", why)
				}
				for range 2 {
					if err := appendBudgetRow(budgetRow{At: now, Kind: kind, ID: strconv.Itoa(i), Tokens: 0}); err != nil {
						t.Fatal(err)
					}
				}
				if err := appendBudgetRow(budgetRow{At: now, Kind: kind, ID: "", Tokens: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if ok, why := allow(kind, now); ok || why != "daily "+kind+" count limit reached" {
				t.Fatalf("at limit: %v, %q", ok, why)
			}
		})
	}
}

func TestAllowTokenLimit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	for _, kind := range []string{KindBenchLoop, KindGateFix} {
		if err := appendBudgetRow(budgetRow{At: now, Kind: kind, ID: "", Tokens: dailyTokens / 2}); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{KindBenchLoop, KindGateFix} {
		if ok, why := allow(kind, now); ok || why != "daily token limit reached" {
			t.Fatalf("at token limit: %v, %q", ok, why)
		}
	}
}

func TestAllowCorruptLedger(t *testing.T) {
	for _, raw := range []string{"not json\n", "{}\n", "null\n", "\n"} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if err := appendBudgetRow(budgetRow{At: time.Now(), Kind: KindBenchLoop, ID: "one", Tokens: 0}); err != nil {
				t.Fatal(err)
			}
			if err := paths.AppendLine(filepath.Join(benchRoot(), "budget.jsonl"), []byte(raw)); err != nil {
				t.Fatal(err)
			}
			if ok, why := allow(KindBenchLoop, time.Now()); ok || why == "" {
				t.Fatalf("corrupt ledger allowed: %v, %q", ok, why)
			}
		})
	}
}

func TestAllowUnreadableLedger(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(benchRoot(), "budget.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, why := allow(KindBenchLoop, time.Now()); ok || why == "" {
		t.Fatalf("unreadable ledger allowed: %v, %q", ok, why)
	}
}

func TestAllowLocalDayBoundary(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	loc := time.FixedZone("local", -5*60*60)
	before := time.Date(2026, 11, 23, 23, 59, 59, 0, loc)
	if err := appendBudgetRow(budgetRow{At: before.UTC(), Kind: KindBenchLoop, ID: "yesterday", Tokens: dailyTokens}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := allow(KindBenchLoop, before); ok {
		t.Fatal("UTC date bypassed the local day limit")
	}
	if ok, why := allow(KindBenchLoop, before.Add(time.Second)); !ok {
		t.Fatalf("new local day refused: %s", why)
	}
}

func TestReserveAtomicAtLimit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	for i := range dailyBenchLoops - 1 {
		if err := appendBudgetRow(budgetRow{At: now, Kind: KindBenchLoop, ID: "seed" + strconv.Itoa(i), Tokens: 0}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var granted atomic.Int32
	for i := range 8 {
		wg.Go(func() {
			if ok, _ := Reserve(KindBenchLoop, "sha"+strconv.Itoa(i), now); ok {
				granted.Add(1)
			}
		})
	}
	wg.Wait()
	if rows, err := readBudget(); granted.Load() != 1 || err != nil || len(rows) != dailyBenchLoops {
		t.Fatalf("granted %d, ledger %d rows (%v); want 1 and %d", granted.Load(), len(rows), err, dailyBenchLoops)
	}
}

func TestReserveOnePerCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	if ok, why := Reserve(KindBenchLoop, "abc", now); !ok {
		t.Fatalf("first launch refused: %s", why)
	}
	if ok, why := Reserve(KindBenchLoop, "abc", now.AddDate(0, 0, 1)); ok || why != "already launched for this commit" {
		t.Fatalf("second launch: %v, %q", ok, why)
	}
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestAdmitLoopReservationStartsOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	if ok, why := Reserve(KindBenchLoop, testSHA, now); !ok {
		t.Fatal(why)
	}
	if why, err := admitLoop(testSHA, now); why != "" || err != nil {
		t.Fatalf("first start refused: %q, %v", why, err)
	}
	if why, err := admitLoop(testSHA, now); !strings.Contains(why, "already used") || err != nil {
		t.Fatalf("second start: %q, %v", why, err)
	}
	rows, err := readBudget()
	if runs, tokens := dayUse(rows, KindBenchLoop, now); err != nil || runs != 1 || tokens != 0 || len(rows) != 2 {
		t.Fatalf("%d rows, %d runs, %d tokens, %v; want the reservation and its start counted as 1 run", len(rows),
			runs, tokens, err)
	}
}

func TestAdmitLoopReservationRefusals(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name, id, want string
		reservedAt     time.Time
	}{
		{"yesterday's reservation", testSHA, "another day", now.AddDate(0, 0, -1)},
		{"a hand id", "hand-20261123T120000.000000000", "40-hex", now},
		{"no reservation", testSHA, "no launch reservation", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if ok, why := Reserve(KindBenchLoop, c.id, c.reservedAt); !c.reservedAt.IsZero() && !ok {
				t.Fatal(why)
			}
			if why, err := admitLoop(c.id, now); !strings.Contains(why, c.want) || err != nil {
				t.Fatalf("admit %s: %q, %v; want %q", c.id, why, err, c.want)
			}
		})
	}
}

func TestLoopLockedProbeCreatesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(benchRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if loopLocked() {
		t.Fatal("missing lock file reported held")
	}
	if _, err := os.Stat(filepath.Join(benchRoot(), "loop.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe created the lock file: %v", err)
	}
}

func TestSpendDispatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := paths.WriteJSON(filepath.Join(paths.DispatchDir(), "dispatch-one.json"), map[string]any{
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 20, "cache_creation_tokens": 30, "cache_read_tokens": 40},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := SpendDispatch(KindBenchLoop, "dispatch-one", now); err != nil {
		t.Fatal(err)
	}
	rows, err := readBudget()
	if err != nil || len(rows) != 1 || rows[0].Dispatch != "dispatch-one" || rows[0].Tokens != 100 {
		t.Fatalf("dispatch spend: %+v, %v", rows, err)
	}
}

func TestSpendDispatchReplacesAnEarlierAmount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	settled := 0
	old := settleDispatch
	t.Cleanup(func() { settleDispatch = old })
	settleDispatch = func(string) { settled++ }
	write := func(status string, tokens int) {
		if err := paths.WriteJSON(filepath.Join(paths.DispatchDir(), "d1.json"), map[string]any{
			"status": status, "usage": map[string]int{"input_tokens": tokens},
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("running", 10)
	if err := SpendDispatch(KindBenchLoop, "d1", now); err != nil || settled != 1 {
		t.Fatalf("running record: err %v, settle waits %d, want 1", err, settled)
	}
	write("ok", 50)
	if err := SpendDispatch(KindBenchLoop, "d1", now); err != nil || settled != 1 {
		t.Fatalf("settled record: err %v, settle waits %d, want still 1", err, settled)
	}
	rows, err := readBudget()
	if _, tokens := dayUse(rows, KindBenchLoop, now); err != nil || len(rows) != 2 || tokens != 50 {
		t.Fatalf("%d rows, day total %d, %v; want 2 rows counting 50 once", len(rows), tokens, err)
	}
}
