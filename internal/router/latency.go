package router

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/tokenledger"
)

// latencyDim scores the fastest measured blended first-event latency 1 and slower ones proportionally less.
type latencyDim struct{}

func (latencyDim) Name() string { return "latency" }

// Score is fastest/own; an unmeasured candidate ties at 1, as an unpriced one does in priceDim.
func (latencyDim) Score(c Candidate, s pickScope) float64 {
	own, ok := s.Latency.ms(c)
	if !ok {
		return 1
	}
	low := own
	for _, o := range s.Candidates {
		if v, ok := s.Latency.ms(o); ok && v < low {
			low = v
		}
	}
	return float64(low) / float64(own)
}

type latKey struct{ account, model string }

// latencyTable is the blended first-event ms, (p50+p95)/2, per account key and model key.
type latencyTable map[latKey]int64

func (l latencyTable) ms(c Candidate) (int64, bool) {
	v, ok := l[latKey{c.Account, models.ModelKey(c.Model)}]
	return v, ok && v > 0
}

func (l latencyTable) anyMeasured(cs []Candidate) bool {
	return slices.ContainsFunc(cs, func(c Candidate) bool { _, ok := l.ms(c); return ok })
}

// withoutLatency drops the latency weight and rescales the rest to sum 1, so no data scores as before.
func withoutLatency(w map[string]float64) map[string]float64 {
	out := maps.Clone(w)
	delete(out, "latency")
	sum := 0.0
	for _, v := range out {
		sum += v
	}
	for k := range out {
		if sum == 0 {
			out[k] = 1 / float64(len(out))
		} else {
			out[k] /= sum
		}
	}
	return out
}

const (
	latencyMinSamples = 5
	latencyWindow     = 7 * 24 * time.Hour
	latencyCacheTTL   = 30 * time.Second
)

// loadLatency reads the ledger into a latencyTable; a var so tests inject rows, nil under test by default.
var loadLatency = func(now time.Time) latencyTable {
	if testing.Testing() {
		return nil
	}
	stats, err := tokenledger.LatencyStats(now.Add(-latencyWindow))
	if err != nil {
		return nil
	}
	return latencyFromStats(stats)
}

// latencyFromStats keeps keys with enough samples; on a key collision more samples win, then the lower ms.
func latencyFromStats(stats map[tokenledger.LatencyKey]tokenledger.Latency) latencyTable {
	out, n := latencyTable{}, map[latKey]int{}
	for k, v := range stats {
		if v.N < latencyMinSamples {
			continue
		}
		key := latKey{cmp.Or(k.Provider, "databricks") + "·" + k.Profile, models.ModelKey(k.Model)}
		ms := (v.FirstP50 + v.FirstP95) / 2
		if v.N > n[key] || v.N == n[key] && ms < out[key] {
			n[key], out[key] = v.N, ms
		}
	}
	return out
}

var latencyMemo struct {
	sync.Mutex
	at  time.Time
	tab latencyTable
}

// latencyFor is loadLatency's table, memoised for latencyCacheTTL.
func latencyFor(now time.Time) latencyTable {
	m := &latencyMemo
	m.Lock()
	defer m.Unlock()
	if !m.at.IsZero() && now.Sub(m.at) >= 0 && now.Sub(m.at) < latencyCacheTTL {
		return m.tab
	}
	m.tab, m.at = loadLatency(now), now
	return m.tab
}
