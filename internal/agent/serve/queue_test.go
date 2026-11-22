package serve

import (
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
)

// wantGranted fails unless exactly the waiters marked true have their grant closed.
func wantGranted(t *testing.T, step string, ws []*waiter, want ...bool) {
	t.Helper()
	got := make([]bool, len(ws))
	for i, w := range ws {
		select {
		case <-w.grant:
			got[i] = true
		default:
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: grants = %v, want %v", step, got, want)
	}
}

func TestEnqueueRejectsDuplicateID(t *testing.T) {
	now := time.Now()
	q := NewQueue(Sources{
		Running: func() map[string][]string { return map[string][]string{"pool": {"a", "b"}} },
		Cap:     func(string) int { return 1 },
		CoolEnd: func(string, time.Time) time.Time { return time.Time{} },
	}, 1)
	if _, _, err := q.Enqueue("a", "pool", now); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{"pool", "other"} {
		if _, _, err := q.Enqueue("a", pool, now); err == nil || q.Len() != 1 {
			t.Fatal("duplicate ID occupied another queue slot")
		}
	}
	if _, _, err := q.Enqueue("b", "pool", now); err != nil {
		t.Fatal(err)
	}
	if n := q.Admit(now); n != 1 {
		t.Fatalf("admitted %d, want 1", n)
	}
	if _, _, err := q.Enqueue("a", "pool", now); err == nil {
		t.Fatal("already-admitted ID was queued again")
	}
	if n := q.Admit(now); n != 0 {
		t.Fatalf("duplicate admission let %d extra jobs bypass cap 1", n)
	}
}

func TestCoolEndIgnoresUsageExhaustion(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	if err := router.Record("test", 100, "weekly", now.Add(7*24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if end := coolEnd("test", now); !end.IsZero() {
		t.Fatalf("usage observation held queue until %s", end)
	}
	until := now.Add(time.Second)
	if ran, err := router.MarkCooling("test", until, now); err != nil || !ran {
		t.Fatalf("MarkCooling = %v, %v", ran, err)
	}
	if end := coolEnd("test", now); !end.Equal(until) {
		t.Fatalf("coolEnd = %s, want actual 429 hold %s", end, until)
	}
}

func TestAdmitOrderUnderPoolCap(t *testing.T) {
	now := time.Now()
	running := map[string][]string{}
	cool := map[string]time.Time{"databricks·qa": now.Add(time.Minute)}
	q := NewQueue(Sources{
		Running: func() map[string][]string { return running },
		Cap:     func(string) int { return 24 },
		CoolEnd: func(pool string, _ time.Time) time.Time { return cool[pool] },
	}, 1)
	var ws []*waiter
	for _, j := range [][2]string{{"a", "databricks·dev"}, {"b", "databricks·dev"}, {"c", "databricks·qa"},
		{"d", "anthropic·ultra"}} {
		w, _, err := q.Enqueue(j[0], j[1], now)
		if err != nil {
			t.Fatal(err)
		}
		ws = append(ws, w)
	}
	running["databricks·dev"] = []string{"a", "b"} // a queued job's record says running; only admitted ones count
	q.Admit(now)
	wantGranted(t, "first pass (dev cap 1, qa cooling)", ws, true, false, false, true)
	q.Admit(now)
	wantGranted(t, "a still runs", ws, true, false, false, true)
	running["databricks·dev"] = []string{"b"} // a finished
	q.Admit(now)
	wantGranted(t, "a finished", ws, true, true, false, true)
	delete(cool, "databricks·qa")
	q.Admit(now)
	wantGranted(t, "cool-down over", ws, true, true, true, true)
	if q.Len() != 0 {
		t.Fatalf("queue left %d", q.Len())
	}
}
