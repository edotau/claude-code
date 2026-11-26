package tokenledger

import (
	"math"
	"slices"
	"time"
)

// LatencyKey is one served row and model as the ledger spells them.
type LatencyKey struct{ Provider, Profile, Model string }

// Latency is the nearest-rank p50/p95 in ms over N rows that carry first_ms.
type Latency struct {
	N                  int
	FirstP50, FirstP95 int64
	TotalP50, TotalP95 int64
}

// latencyTail bounds the ledger bytes a latency read scans, from the end; a var so tests can shrink it.
var latencyTail int64 = 2 << 20

type samples struct{ first, total []int64 }

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
	})
	return summarize(got), err
}

func summarize(got map[LatencyKey]*samples) map[LatencyKey]Latency {
	out := make(map[LatencyKey]Latency, len(got))
	for k, s := range got {
		slices.Sort(s.first)
		slices.Sort(s.total)
		out[k] = Latency{N: len(s.first), FirstP50: rank(s.first, 0.5), FirstP95: rank(s.first, 0.95),
			TotalP50: rank(s.total, 0.5), TotalP95: rank(s.total, 0.95)}
	}
	return out
}

// rank is the nearest-rank percentile v[ceil(p*n)-1] of a sorted slice.
func rank(v []int64, p float64) int64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
