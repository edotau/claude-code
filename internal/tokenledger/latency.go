package tokenledger

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// LatencyKey is one served row and model as the ledger spells them.
type LatencyKey struct{ Provider, Profile, Model string }

// Latency is the nearest-rank p50/p95 in ms over N rows that carry first_ms, plus median decode throughput.
type Latency struct {
	N                  int
	FirstP50, FirstP95 int64
	TotalP50, TotalP95 int64
	OutTokPerSecP50    float64
}

// EffectiveMs estimates turn latency (TTFT + refOutput / decode tok/s), falling back to TTFT when unmeasured.
func (l Latency) EffectiveMs(refOutput int) int64 {
	ttft := (l.FirstP50 + l.FirstP95) / 2
	if refOutput <= 0 || l.OutTokPerSecP50 <= 0 {
		return ttft
	}
	return ttft + int64(math.Round(float64(refOutput)*1000.0/l.OutTokPerSecP50))
}

// minThroughputOutput skips tiny replies whose decode window is dominated by framing overhead.
const minThroughputOutput = 32

// latencyTail bounds the ledger bytes a latency read scans, from the end; a var so tests can shrink it.
var latencyTail int64 = 2 << 20

type samples struct {
	first, total []int64
	tps          []float64
}

// LatencyStats reads the ledger's last latencyTail bytes; rows before since or without first_ms are skipped.
func LatencyStats(since time.Time) (map[LatencyKey]Latency, error) {
	got := map[LatencyKey]*samples{}
	err := readMatching([]byte(`"first_ms"`), latencyTail, func(e Entry) {
		if e.FirstMs <= 0 || e.Ts.Before(since) {
			return
		}
		k := LatencyKey{e.Provider, e.ProfileName(), e.Model}
		if got[k] == nil {
			got[k] = &samples{}
		}
		s := got[k]
		s.first = append(s.first, e.FirstMs)
		s.total = append(s.total, e.TotalMs)
		if decodeMs := e.TotalMs - e.FirstMs; decodeMs > 0 && e.Output >= minThroughputOutput {
			s.tps = append(s.tps, float64(e.Output)*1000.0/float64(decodeMs))
		}
	})
	return summarize(got), err
}

func summarize(got map[LatencyKey]*samples) map[LatencyKey]Latency {
	out := make(map[LatencyKey]Latency, len(got))
	for k, s := range got {
		slices.Sort(s.first)
		slices.Sort(s.total)
		var tpsP50 float64
		if len(s.tps) > 0 {
			slices.Sort(s.tps)
			tpsP50 = rank(s.tps, 0.5)
		}
		out[k] = Latency{N: len(s.first), FirstP50: rank(s.first, 0.5), FirstP95: rank(s.first, 0.95),
			TotalP50: rank(s.total, 0.5), TotalP95: rank(s.total, 0.95), OutTokPerSecP50: tpsP50}
	}
	return out
}

// rank is the nearest-rank percentile v[ceil(p*n)-1] of a sorted slice.
func rank[T cmp.Ordered](v []T, p float64) T { return v[int(math.Ceil(p*float64(len(v))))-1] }
