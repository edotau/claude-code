package paths

import (
	"sort"
	"sync"
	"sync/atomic"
)

// CacheStat is one named cache's counters in this process.
type CacheStat struct {
	Name                               string
	Hits, Misses, Refreshes, Evictions uint64
}

// cacheStats are a cache's atomic counters; a nil *cacheStats (an unnamed zero-value cache) counts nothing.
type cacheStats struct {
	name                               string
	hits, misses, refreshes, evictions atomic.Uint64
}

var (
	statsMu  sync.Mutex
	statsReg = map[string]*cacheStats{}
)

// registerStats returns the counters for name; a repeated name shares one set.
func registerStats(name string) *cacheStats {
	statsMu.Lock()
	defer statsMu.Unlock()
	s := statsReg[name]
	if s == nil {
		s = &cacheStats{name: name}
		statsReg[name] = s
	}
	return s
}

func (s *cacheStats) hit() {
	if s != nil {
		s.hits.Add(1)
	}
}

func (s *cacheStats) miss() {
	if s != nil {
		s.misses.Add(1)
	}
}

func (s *cacheStats) refresh() {
	if s != nil {
		s.refreshes.Add(1)
	}
}

func (s *cacheStats) evict(n int) {
	if s != nil && n > 0 {
		s.evictions.Add(uint64(n))
	}
}

// CacheStats returns the non-zero counters of every named cache in this process, sorted by name.
func CacheStats() []CacheStat {
	statsMu.Lock()
	defer statsMu.Unlock()
	var out []CacheStat
	for _, s := range statsReg {
		c := CacheStat{s.name, s.hits.Load(), s.misses.Load(), s.refreshes.Load(), s.evictions.Load()}
		if c.Hits|c.Misses|c.Refreshes|c.Evictions != 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
