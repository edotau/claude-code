package router

import (
	"math"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/tokenledger"
)

func setLatency(t *testing.T, tab latencyTable) {
	old := loadLatency
	loadLatency = func(time.Time) latencyTable { return tab }
	resetLatencyMemo()
	t.Cleanup(func() { loadLatency = old; resetLatencyMemo() })
}

// resetLatencyMemo drops the memo so the next pick reloads.
func resetLatencyMemo() {
	latencyMemo.Lock()
	latencyMemo.at = time.Time{}
	latencyMemo.Unlock()
}

func latCands() []Candidate {
	var a, b Candidate
	a.Agent, a.Account, a.Model = "a", "acc·a", "m"
	b.Agent, b.Account, b.Model = "b", "acc·b", "m"
	return []Candidate{a, b}
}

func TestLatencyNoDataKeepsTotals(t *testing.T) {
	setLatency(t, nil)
	now := time.Now()
	three := &RouteTable{Weights: map[string]float64{"price": 0.4, "headroom": 0.4, "latency": 0.2}}
	two := &RouteTable{Weights: map[string]float64{"price": 0.5, "headroom": 0.5}}
	got := three.scoreVia(noAcct, latCands(), State{}, now)
	want := two.scoreVia(noAcct, latCands(), State{}, now)
	for i := range got {
		if got[i].Total != want[i].Total {
			t.Errorf("total %d = %v, want %v", i, got[i].Total, want[i].Total)
		}
	}
}

func TestLatencyFasterRowWins(t *testing.T) {
	setLatency(t, latencyTable{{"acc·a", "m"}: 400, {"acc·b", "m"}: 1200})
	tb := &RouteTable{Weights: map[string]float64{"price": 0.4, "headroom": 0.4, "latency": 0.2}}
	got := tb.scoreVia(noAcct, latCands(), State{}, time.Now())
	if s := got[0].Scores["latency"]; s != 1 {
		t.Errorf("fast score = %v, want 1", s)
	}
	if s := got[1].Scores["latency"]; math.Abs(s-1.0/3) > 1e-9 {
		t.Errorf("slow score = %v, want 1/3", s)
	}
	if got[0].Total <= got[1].Total {
		t.Errorf("totals %v <= %v, fast should lead", got[0].Total, got[1].Total)
	}
}

func noAcct(p string) string { return p }

func TestLatencyFromStats(t *testing.T) {
	k := tokenledger.LatencyKey{Provider: "", Profile: "dev", Model: "m"}
	got := latencyFromStats(map[tokenledger.LatencyKey]tokenledger.Latency{
		k: {N: 5, FirstP50: 100, FirstP95: 300},
		{Provider: "x", Profile: "p", Model: "m"}: {N: 4, FirstP50: 1, FirstP95: 1},
	})
	if len(got) != 1 || got[latKey{"databricks·dev", "m"}] != 200 {
		t.Errorf("table = %v, want one databricks·dev row at 200", got)
	}
}

func TestLatencyFallbacks(t *testing.T) {
	w := withoutLatency(map[string]float64{"price": 0, "headroom": 0, "latency": 1})
	if w["price"] != 0.5 || w["headroom"] != 0.5 || len(w) != 2 {
		t.Errorf("zero-rest weights = %v, want 0.5/0.5", w)
	}
	a := tokenledger.LatencyKey{Provider: "p", Profile: "x", Model: "m"}
	b := a
	b.Model = "M"
	got := latencyFromStats(map[tokenledger.LatencyKey]tokenledger.Latency{
		a: {N: 5, FirstP50: 300, FirstP95: 300}, b: {N: 5, FirstP50: 100, FirstP95: 100},
	})
	if got[latKey{"p·x", "m"}] != 100 {
		t.Errorf("collision = %v, want the lower ms", got)
	}
	setLatency(t, nil)
	now := time.Now()
	latencyFor(now)
	loadLatency = func(time.Time) latencyTable { return latencyTable{{"a", "m"}: 1} }
	if len(latencyFor(now.Add(-time.Minute))) != 1 {
		t.Error("a now before the memo must reload")
	}
}

func TestLatencyThroughputOverridesTTFT(t *testing.T) {
	slow := tokenledger.LatencyKey{Provider: "ws", Profile: "a", Model: "m"}
	fast := tokenledger.LatencyKey{Provider: "direct", Profile: "b", Model: "m"}
	unmeasured := tokenledger.LatencyKey{Provider: "other", Profile: "c", Model: "m"}
	got := latencyFromStats(map[tokenledger.LatencyKey]tokenledger.Latency{
		slow:       {N: 10, FirstP50: 350, FirstP95: 450, OutTokPerSecP50: 50},
		fast:       {N: 10, FirstP50: 1500, FirstP95: 1900, OutTokPerSecP50: 200},
		unmeasured: {N: 10, FirstP50: 1000, FirstP95: 1000},
	})
	if got[latKey{"direct·b", "m"}] >= got[latKey{"ws·a", "m"}] {
		t.Fatalf("expected faster decode route to have lower effective ms: got %v", got)
	}
	if got[latKey{"other·c", "m"}] <= got[latKey{"direct·b", "m"}] {
		t.Fatalf("unmeasured throughput row must not undercut measured faster route via TTFT-only scale: got %v", got)
	}
}

func TestStickyOrderAndDefaultEffort(t *testing.T) {
	a := StickyOrder("repo-x", []string{"ws-a", "ws-b", "ws-c"})
	b := StickyOrder("repo-x", []string{"ws-c", "ws-a", "ws-b"})
	if len(a) != 3 || a[0] != b[0] || a[1] != b[1] || a[2] != b[2] {
		t.Fatalf("StickyOrder not deterministic across input permutations: %v vs %v", a, b)
	}
	if DefaultEffortForTier("fast") != "low" || DefaultEffortForTier("standard") != "low" || DefaultEffortForTier("deep") != "high" {
		t.Fatalf("unexpected DefaultEffortForTier mapping")
	}
}
