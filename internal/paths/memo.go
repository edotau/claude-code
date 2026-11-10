package paths

import (
	"sync"
	"time"
)

// memoNow is Memo's clock; tests advance it.
var memoNow = time.Now

// Memo remembers fn's answer per key (keep: 0 = until Drop or evicted past the cap, <0 = not at all); one fn per key runs at a time.
type Memo[K comparable, V any] struct {
	mu      sync.Mutex
	m       map[K]*memoEntry[V]
	sweepAt int    // map size that triggers the next sweep of expired entries
	seq     uint64 // settle counter: orders entries by when they settled
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
	m.mu.Lock()
	if e := m.m[key]; e != nil {
		if !e.settled {
			m.mu.Unlock()
			<-e.done // waiters share the run's answer whatever keep says
			return e.v, e.err
		}
		if e.until.IsZero() || memoNow().Before(e.until) {
			defer m.mu.Unlock()
			return e.v, e.err
		}
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
	e := &memoEntry[V]{done: make(chan struct{})}
	m.m[key] = e
	m.mu.Unlock()

	v, err := fn()
	d := keep(v, err)

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
	return v, err
}

// sweep drops expired entries (zero until = in flight or keep 0, never swept), then waits for the map to double.
func (m *Memo[K, V]) sweep() {
	now := memoNow()
	for k, e := range m.m {
		if !e.until.IsZero() && !now.Before(e.until) {
			delete(m.m, k)
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
	}
}

// Drop forgets every kept answer; runs in flight still answer their waiters but are not kept.
func (m *Memo[K, V]) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m = nil
}
