package router

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/dev/tokenledger"
	"github.com/edotau/claude-code/internal/routing/models"
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
	latencyRefOutput  = 1000
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

// latencyFromStats keeps keys with enough samples; when any row measures decode tok/s, unmeasured rows use the median.
func latencyFromStats(stats map[tokenledger.LatencyKey]tokenledger.Latency) latencyTable {
	out, n := latencyTable{}, map[latKey]int{}
	fallbackTPS := medianMeasuredTPS(stats)
	for k, v := range stats {
		if v.N < latencyMinSamples {
			continue
		}
		key := latKey{cmp.Or(k.Provider, "databricks") + "·" + k.Profile, models.ModelKey(k.Model)}
		if v.OutTokPerSecP50 <= 0 && fallbackTPS > 0 {
			v.OutTokPerSecP50 = fallbackTPS
		}
		ms := v.EffectiveMs(latencyRefOutput)
		if v.N > n[key] || v.N == n[key] && ms < out[key] {
			n[key], out[key] = v.N, ms
		}
	}
	return out
}

func medianMeasuredTPS(stats map[tokenledger.LatencyKey]tokenledger.Latency) float64 {
	var tps []float64
	for _, v := range stats {
		if v.N >= latencyMinSamples && v.OutTokPerSecP50 > 0 {
			tps = append(tps, v.OutTokPerSecP50)
		}
	}
	if len(tps) == 0 {
		return 0
	}
	slices.Sort(tps)
	mid := len(tps) / 2
	if len(tps)%2 == 0 {
		return (tps[mid-1] + tps[mid]) / 2
	}
	return tps[mid]
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
