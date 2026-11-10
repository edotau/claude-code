package paths

import (
	"sync"
	"time"
)

// memoNow is Memo's clock; tests advance it.
var memoNow = time.Now

// Memo remembers fn's answer per key (keep: 0 = until Drop, <0 = not at all); one fn per key runs at a time.
type Memo[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*memoEntry[V]
}

// memoEntry is one key's run: in flight until done closes, then its answer, kept until `until` (zero = forever).
type memoEntry[V any] struct {
	done    chan struct{}
	settled bool
	v       V
	err     error
	until   time.Time
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
	e := &memoEntry[V]{done: make(chan struct{})}
	m.m[key] = e
	m.mu.Unlock()

	v, err := fn()
	d := keep(v, err)

	m.mu.Lock()
	e.v, e.err, e.settled = v, err, true
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

// Drop forgets every kept answer; runs in flight still answer their waiters but are not kept.
func (m *Memo[K, V]) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m = nil
}
