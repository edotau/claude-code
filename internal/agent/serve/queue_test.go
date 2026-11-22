package serve

import (
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
)

func TestAdmitReadsLimitsOncePerPoolPerPass(t *testing.T) {
	now := time.Now()
	caps, cools := 0, 0
	q := NewQueue(Sources{
		Running: func() map[string][]string { return nil },
		Cap:     func(string) int { caps++; return 1 },
		CoolEnd: func(string, time.Time) time.Time { cools++; return now.Add(time.Second) },
	}, 0)
	for i := range MaxQueue {
		if _, _, err := q.Enqueue(strconv.Itoa(i), strconv.Itoa(i%2), now); err != nil {
			t.Fatal(err)
		}
	}
	if got := q.Admit(now); got != 0 || caps != 2 || cools != 2 {
		t.Fatalf("grants/cap reads/cool reads = %d/%d/%d, want 0/2/2", got, caps, cools)
	}
	if got := q.Admit(now.Add(2 * time.Second)); got != 2 || caps != 4 || cools != 4 {
		t.Fatalf("next pass = %d/%d/%d, want 2/4/4", got, caps, cools)
	}
}

func TestRunningByPoolExcludesCoordinators(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var worker agent.Record
	for i, name := range []string{"waves", "claude", "claude"} {
		r := agent.NewRecord(name, agent.RunOptions{Provider: "test", Carry: i == 1}, time.Now().Add(time.Duration(i)*time.Millisecond))
		r.Status, r.PID = "running", os.Getpid()
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
		worker = r
	}
	got := runningByPool()
	if len(got) != 1 || !slices.Equal(got[PoolOf(worker)], []string{worker.ID}) {
		t.Fatalf("model load includes orchestration records: %v", got)
	}
}

func TestRunningByPoolCountsCarryOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	outer := agent.NewRecord("codex", agent.RunOptions{Provider: "test", Carry: true}, time.Now())
	outer.Status, outer.PID, outer.Background = "running", os.Getpid(), true
	child := agent.NewRecord("codex", agent.RunOptions{Provider: "test"}, time.Now().Add(time.Millisecond))
	child.Parent, child.Status, child.PID = outer.ID, "running", os.Getpid()
	for _, phase := range []string{"before child", "with child", "child finished", "takeover", "finished"} {
		switch phase {
		case "with child", "takeover":
			child.Status = "running"
			if phase == "takeover" {
				child.Agent = "pi"
			}
		case "child finished":
			child.Status = "ok"
		case "finished":
			outer.Status, child.Status = "ok", "ok"
		}
		if err := outer.Save(); err != nil {
			t.Fatal(err)
		}
		if phase != "before child" {
			if err := child.Save(); err != nil {
				t.Fatal(err)
			}
		}
		ids := runningByPool()[PoolOf(outer)]
		want := outer.ID
		if phase == "with child" || phase == "takeover" {
			want = child.ID
		}
		if phase == "finished" {
			if len(ids) != 0 {
				t.Fatalf("finished carry still counted: %v", ids)
			}
		} else if !slices.Equal(ids, []string{want}) {
			t.Fatalf("%s: counted %v, want [%s]", phase, ids, want)
		}
	}
}

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
