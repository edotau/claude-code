package paths

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errMemoPanic = errors.New("paths: memoized function panicked")

var memoNow = time.Now

// Memo remembers fn's answer per key (keep: 0 = until Drop or evicted, <0 = not at all); one fn per key runs at a time.
type Memo[K comparable, V any] struct {
	mu      sync.Mutex
	m       map[K]*memoEntry[V]
	sweepAt int    // map size that triggers the next sweep of expired entries
	seq     uint64 // settle counter: orders entries by when they settled
	stats   *cacheStats
}

// NewMemo returns a Memo whose hit, miss, refresh and eviction counts show in CacheStats under name.
func NewMemo[K comparable, V any](name string) *Memo[K, V] {
	return &Memo[K, V]{stats: registerStats(name)}
}

// memoSweepMin is the slack added after each sweep: a map never sweeps before it holds this many more entries.
const memoSweepMin = 8

// memoCap bounds the settled entries a Memo holds; the oldest settled go first, keep-forever ones included.
const memoCap = 64

// memoEntry is one key's run: in flight until done closes, then its answer, kept until `until` (zero = forever).
type memoEntry[V any] struct {
	done    chan struct{}
	settled bool
	v       V
	err     error
	until   time.Time
	order   uint64 // m.seq when it settled
}

// Do returns key's kept answer, joins the run in flight for key, or runs fn and keeps its answer per keep.
func (m *Memo[K, V]) Do(key K, keep func(V, error) time.Duration, fn func() (V, error)) (V, error) {
	e, joined := m.claim(key)
	if joined {
		<-e.done // waiters share the run's answer whatever keep says
		return e.v, e.err
	}
	returned := false
	defer func() { // a panic in fn or keep must not leave the entry in flight: joiners and later calls would block
		if !returned {
			m.abandon(key, e)
		}
	}()
	v, err := fn()
	d := keep(v, err)
	returned = true
	m.settle(key, e, v, err, d)
	return v, err
}

// claim returns key's kept or in-flight entry (joined), else a fresh in-flight entry this caller must run.
func (m *Memo[K, V]) claim(key K) (e *memoEntry[V], joined bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.m[key]
	if old != nil && (!old.settled || old.until.IsZero() || memoNow().Before(old.until)) {
		m.stats.hit()
		return old, true
	}
	m.stats.miss()
	if old != nil {
		m.stats.refresh()
	}
	if m.m == nil {
		m.m = map[K]*memoEntry[V]{}
	}
	if len(m.m) >= m.sweepAt {
		m.sweep()
	}
	if m.m[key] == nil {
		m.evict()
	}
	e = &memoEntry[V]{done: make(chan struct{})}
	m.m[key] = e
	return e, false
}

// abandon removes a panicked run's entry and releases its joiners with errMemoPanic.
func (m *Memo[K, V]) abandon(key K, e *memoEntry[V]) {
	m.mu.Lock()
	if m.m[key] == e {
		delete(m.m, key)
	}
	m.mu.Unlock()
	e.err = errMemoPanic
	close(e.done)
}

// settle records e's answer, keeps it for d (0 = until Drop or evicted, <0 = not at all) and releases joiners.
func (m *Memo[K, V]) settle(key K, e *memoEntry[V], v V, err error, d time.Duration) {
	m.mu.Lock()
	m.seq++
	e.v, e.err, e.settled, e.order = v, err, true, m.seq
	if d > 0 {
		e.until = memoNow().Add(d)
	}
	if d < 0 && m.m[key] == e { // a run started before Drop is already gone from m.m, so never kept
		delete(m.m, key)
	}
	m.mu.Unlock()
	close(e.done)
}

// sweep drops expired entries (zero until = in flight or keep 0, never swept), then waits for the map to double.
func (m *Memo[K, V]) sweep() {
	now := memoNow()
	for k, e := range m.m {
		if !e.until.IsZero() && !now.Before(e.until) {
			delete(m.m, k)
			m.stats.evict(1)
		}
	}
	m.sweepAt = 2*len(m.m) + memoSweepMin
}

// evict makes room for one more entry: it drops the oldest settled entries until the map is under memoCap.
func (m *Memo[K, V]) evict() {
	for len(m.m) >= memoCap {
		var oldest K
		var found *memoEntry[V]
		for k, e := range m.m {
			if e.settled && (found == nil || e.order < found.order) {
				oldest, found = k, e
			}
		}
		if found == nil {
			return // every entry is a run in flight
		}
		delete(m.m, oldest)
		m.stats.evict(1)
	}
}

// Drop forgets every kept answer; runs in flight still answer their waiters but are not kept.
func (m *Memo[K, V]) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m = nil
}

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

// CacheSummary renders CacheStats as "name=hits/misses/refreshes/evictions ..."; empty when nothing counted.
func CacheSummary() string {
	var b strings.Builder
	for _, c := range CacheStats() {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%d/%d/%d/%d", c.Name, c.Hits, c.Misses, c.Refreshes, c.Evictions)
	}
	return b.String()
}

// DiskTTL caches one fetched value in Path for TTL (±Jitter), one process fetching while the others wait.
type DiskTTL[T any] struct {
	Name   string
	Path   string
	TTL    time.Duration
	Jitter time.Duration
	Decode func([]byte) (T, error)
	Encode func(T) ([]byte, error)

	once  sync.Once
	stats *cacheStats
}

// Get returns the fresh file's value, else fetches under Path's lock; fetch errors are never written.
func (d *DiskTTL[T]) Get(ctx context.Context, fetch func(context.Context) (T, error)) (T, error) {
	d.once.Do(func() { d.stats = registerStats(d.Name) })
	if v, ok, _ := d.read(); ok {
		d.stats.hit()
		return v, nil
	}
	var out T
	var ferr error
	fetched := false
	lerr := WithFileLock(d.Path, func() error {
		v, ok, existed := d.read()
		if ok {
			out = v
			d.stats.hit()
			return nil
		}
		d.stats.miss()
		if existed {
			d.stats.refresh()
		}
		fetched = true
		if out, ferr = fetch(ctx); ferr == nil {
			d.write(out)
		}
		return nil
	})
	if lerr != nil && !fetched { // lock failure never fails closed
		d.stats.miss()
		return fetch(ctx)
	}
	return out, ferr
}

// read decodes the file when it is fresh; existed reports a file that was there but stale or unreadable.
func (d *DiskTTL[T]) read() (v T, fresh, existed bool) {
	fi, err := os.Stat(d.Path)
	if err != nil {
		return v, false, false
	}
	if Stale(fi.ModTime(), d.ttlFor(fi.ModTime()), RacyWindow, stampNow()) {
		return v, false, true
	}
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return v, false, true
	}
	v, err = d.Decode(data)
	return v, err == nil, true
}

// ttlFor is TTL shifted within ±Jitter by a hash of mtime, so concurrent readers agree on one expiry.
func (d *DiskTTL[T]) ttlFor(mtime time.Time) time.Duration {
	if d.Jitter <= 0 {
		return d.TTL
	}
	h := uint64(mtime.UnixNano()) * 0x9E3779B97F4A7C15
	return d.TTL + time.Duration((h>>11)%uint64(2*d.Jitter+1)) - d.Jitter
}

func (d *DiskTTL[T]) write(v T) {
	if data, err := d.Encode(v); err == nil {
		_ = AtomicWrite(d.Path, data, 0o600)
	}
}
