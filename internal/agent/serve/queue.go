package serve

import (
	"cmp"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// MaxQueue bounds the jobs waiting at once; a submit past it is refused and runs directly.
const MaxQueue = 256

// ErrQueueFull refuses a submit while MaxQueue jobs already wait.
var ErrQueueFull = errors.New("queue full")

// Sources is what admission reads: running dispatches by pool, each pool's cap and its cool-down end.
type Sources struct {
	Running func() map[string][]string
	Cap     func(pool string) int
	CoolEnd func(pool string, now time.Time) time.Time
}

// waiter is one queued job; grant closes when the job may start.
type waiter struct {
	id, pool string
	since    time.Time
	grant    chan struct{}
}

// Queue is the FIFO of jobs waiting for their pool; it keeps no ledger, load is read from the records.
type Queue struct {
	mu       sync.Mutex
	waiting  []*waiter
	admitted map[string]bool
	cap      int // ceiling on every pool's cap; 0 = the account limit alone
	src      Sources
}

// NewQueue is a Queue over src; ceiling > 0 caps every pool at most at ceiling.
func NewQueue(src Sources, ceiling int) *Queue { return &Queue{src: src, cap: ceiling} }

// Enqueue adds job id on pool and returns its waiter and how many wait ahead of it on that pool.
func (q *Queue) Enqueue(id, pool string, now time.Time) (*waiter, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) >= MaxQueue {
		return nil, 0, ErrQueueFull
	}
	ahead := 0
	if q.admitted[id] {
		return nil, 0, errors.New("job already admitted")
	}
	for _, w := range q.waiting {
		if w.id == id {
			return nil, 0, errors.New("job already queued")
		}
		if w.pool == pool {
			ahead++
		}
	}
	w := &waiter{id: id, pool: pool, since: now, grant: make(chan struct{})}
	q.waiting = append(q.waiting, w)
	return w, ahead, nil
}

// Remove drops w, a job that left the queue (gone, cancelled or requeued); a granted w is already gone.
func (q *Queue) Remove(w *waiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.waiting = slices.DeleteFunc(q.waiting, func(x *waiter) bool { return x == w })
}

// Len is the number of jobs waiting.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// capOf is pool's cap: the account limit, lowered to the queue's ceiling.
func (q *Queue) capOf(pool string) int {
	n := q.src.Cap(pool)
	if q.cap > 0 && (n <= 0 || q.cap < n) {
		return q.cap
	}
	return n
}

// Admit grants, oldest first, each waiting job whose pool is below its cap and not cooling; other pools never block.
func (q *Queue) Admit(now time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) == 0 {
		return 0
	}
	load := q.loadLocked()
	if q.admitted == nil {
		q.admitted = make(map[string]bool)
	}
	keep, granted := q.waiting[:0:0], 0
	remaining := make(map[string]int)
	for _, w := range q.waiting {
		if _, ok := remaining[w.pool]; !ok {
			remaining[w.pool] = q.capOf(w.pool) - load[w.pool]
			if q.src.CoolEnd(w.pool, now).After(now) {
				remaining[w.pool] = 0
			}
		}
		if remaining[w.pool] <= 0 {
			keep = append(keep, w)
			continue
		}
		remaining[w.pool]--
		q.admitted[w.id] = true
		granted++
		close(w.grant)
	}
	q.waiting = keep
	return granted
}

// loadLocked counts each pool's running dispatches, less the ones still waiting here (their records say running).
func (q *Queue) loadLocked() map[string]int {
	queued := map[string]bool{}
	for _, w := range q.waiting {
		queued[w.id] = true
	}
	load := map[string]int{}
	active := map[string]bool{}
	for pool, ids := range q.src.Running() {
		for _, id := range ids {
			active[id] = true
			if !queued[id] {
				load[pool]++
			}
		}
	}
	for id := range q.admitted {
		if !active[id] {
			delete(q.admitted, id)
		}
	}
	return load
}

// PoolRow is one pool in the queue view.
type PoolRow struct {
	Pool    string    `json:"pool"`
	Running int       `json:"running"`
	Cap     int       `json:"cap"`
	Cooling time.Time `json:"cooling_until,omitzero"`
	Waiting []Waiter  `json:"waiting,omitempty"`
}

// Waiter is one queued job in the queue view.
type Waiter struct {
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
}

// View is every pool with a running or waiting job, sorted by pool.
func (q *Queue) View(now time.Time) []PoolRow {
	q.mu.Lock()
	defer q.mu.Unlock()
	rows := map[string]*PoolRow{}
	row := func(pool string) *PoolRow {
		if rows[pool] == nil {
			rows[pool] = &PoolRow{Pool: pool, Cap: q.capOf(pool)}
			if end := q.src.CoolEnd(pool, now); end.After(now) {
				rows[pool].Cooling = end
			}
		}
		return rows[pool]
	}
	for pool, n := range q.loadLocked() {
		row(pool).Running = n
	}
	for _, w := range q.waiting {
		r := row(w.pool)
		r.Waiting = append(r.Waiting, Waiter{ID: w.id, Since: w.since})
	}
	out := make([]PoolRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b PoolRow) int { return cmp.Compare(a.Pool, b.Pool) })
	return out
}

// PoolOf is rec's admission pool: the account key of the profile it runs on, the registry default when unrouted.
func PoolOf(rec agent.Record) string {
	profile := cmp.Or(rec.Route.Profile, rec.Request.Lead, rec.Request.Provider, rec.Provider)
	if profile == "" {
		if reg, err := providers.LoadShared(); err == nil {
			profile = reg.Default
		}
	}
	return router.ProviderAccount(cmp.Or(profile, "default"))
}

// RecordSources reads load from running dispatch records, caps from router limits, cool-down from router state.
func RecordSources() Sources {
	return Sources{
		Running: runningByPool,
		Cap:     func(pool string) int { return router.CurrentLimits().For(pool) },
		CoolEnd: coolEnd,
	}
}

// runningByPool groups live running dispatch records by pool; an unreadable dispatch dir counts none.
func runningByPool() map[string][]string {
	recs, _ := agent.RunningRecent(agent.AllRecords)
	children := map[string]bool{}
	for _, r := range recs {
		if r.Alive() {
			children[r.Parent] = true
		}
	}
	out := map[string][]string{}
	for _, r := range recs {
		if !r.Live && r.Agent != "waves" && (!r.Request.Carry || r.Background && !children[r.ID]) && r.Alive() {
			pool := PoolOf(r)
			out[pool] = append(out[pool], r.ID)
		}
	}
	return out
}

// coolEnd holds only for a transient 429, never for a usage observation's reset date.
func coolEnd(pool string, _ time.Time) time.Time {
	s := router.LoadState()
	return s.CoolingUntil(pool)
}
