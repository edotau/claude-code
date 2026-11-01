package agent

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/router"
)

// DefaultPerKey caps concurrent dispatches on one billed account (one credential, one rate limit).
const DefaultPerKey = 4

// sharedTryClaim is the machine-wide limiter seam; tests count or stub it.
var sharedTryClaim = inflight.TryClaim

// sharedPoll is how often a waiting dispatch re-checks the shared limiter; a var so tests can shorten it.
var sharedPoll = 250 * time.Millisecond

// dispatchSeq makes each permit's shared marker id unique within the process.
var dispatchSeq atomic.Uint64

// limiter is the one concurrency gate for dispatches: at most jobs run at once, and at most cap per billed account.
// A waiting task holds nothing, so a full account never blocks another.
type limiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	jobs, cap int
	per       map[string]int
	pending   int             // prepared tasks that have not yet reached their first dispatch: each still owes a slot
	limits    inflight.Limits // shared-limiter caps, read once per batch; zero Total = no shared limiter
}

// slot is one prepared task's claim on a global slot until its first dispatch asks for a permit (or parks, or ends).
type slot struct{ settled bool }

func newLimiter(jobs, cap int) *limiter {
	l := &limiter{jobs: jobs, cap: cap, per: map[string]int{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// wait blocks until ok() holds under the lock, or ctx is done; a cancelled ctx wins over a free slot.
func (l *limiter) wait(ctx context.Context, ok func() bool) error {
	defer context.AfterFunc(ctx, func() { l.mu.Lock(); l.cond.Broadcast(); l.mu.Unlock() })()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ok() {
			return nil
		}
		l.cond.Wait()
	}
}

// reserve waits, holding nothing, for a global slot no prepared task is owed, so a task is prepared only when it can
// start; the claim ends at settle.
func (l *limiter) reserve(ctx context.Context) (*slot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.wait(ctx, func() bool { return l.running+l.pending < l.jobs }); err != nil {
		return nil, err
	}
	l.pending++
	return &slot{}, nil
}

func (l *limiter) settleLocked(s *slot) {
	if s != nil && !s.settled {
		s.settled = true
		l.pending--
		l.cond.Broadcast()
	}
}

// settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func settle(ctx context.Context) {
	if l, _ := ctx.Value(limiterKey{}).(*limiter); l != nil {
		s, _ := ctx.Value(slotKey{}).(*slot)
		l.mu.Lock()
		l.settleLocked(s)
		l.mu.Unlock()
	}
}

// acquire waits for a global slot, an account permit and one shared slot; a full shared account releases the local
// permit before the next try, so it never holds a worker another account could use. The release func is idempotent.
func (l *limiter) acquire(ctx context.Context, account string) (func(), error) {
	if l.limits.Total <= 0 {
		return l.acquireLocal(ctx, account)
	}
	id := inflight.DispatchPrefix + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(dispatchSeq.Add(1), 10)
	c := inflight.Claim{Owner: os.Getpid(), ID: id, Account: account, N: 1}
	for {
		freeLocal, err := l.acquireLocal(ctx, account) // returns ctx.Err() first, so a cancelled dispatch claims nothing
		if err != nil {
			return nil, err
		}
		if ok, _ := sharedTryClaim(c, l.limits); ok {
			var once sync.Once
			return func() { once.Do(func() { inflight.Release(c.Owner, c.ID); freeLocal() }) }, nil
		}
		freeLocal()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sharedPoll):
		}
	}
}

// acquireLocal is the in-process admit: a global slot and, when account is set, an account permit.
func (l *limiter) acquireLocal(ctx context.Context, account string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, _ := ctx.Value(slotKey{}).(*slot)
	l.settleLocked(s) // a parked task no longer owes a slot; an admitted one takes it in this same critical section
	if err := l.wait(ctx, func() bool { return l.running < l.jobs && (account == "" || l.per[account] < l.cap) }); err != nil {
		return nil, err
	}
	l.running++
	if account != "" {
		l.per[account]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			if account != "" {
				l.per[account]--
			}
			l.cond.Broadcast()
		})
	}, nil
}

// permit is a dispatch's current permit; Do swaps it out around retry waits so a backing-off dispatch holds nothing.
type permit struct {
	account string
	release func()
	recheck func() *Error // a skip when the account became unusable since the dispatch started
}

// idle runs wait with the permit released, then takes a permit again; the dispatch runs a child only while holding one.
func (h *permit) idle(ctx context.Context, wait func()) error {
	h.release()
	wait()
	r, err := acquireDispatch(ctx, h.account)
	if err != nil {
		h.release = func() {}
		return err
	}
	h.release = r
	return nil
}

type (
	permitKey   struct{}
	limiterKey  struct{}
	failFastKey struct{}
	slotKey     struct{}
)

func withLimiter(ctx context.Context, l *limiter) context.Context {
	return context.WithValue(ctx, limiterKey{}, l)
}

// acquireDispatch takes a dispatch permit under ctx's limiter; outside a batch there is no limit.
func acquireDispatch(ctx context.Context, account string) (release func(), err error) {
	l, _ := ctx.Value(limiterKey{}).(*limiter)
	if l == nil {
		return func() {}, nil
	}
	return l.acquire(ctx, account)
}

// tripFailFast cancels the batch when err ends the task (no fallback follows), before the dispatch's permit is released.
func tripFailFast(ctx context.Context, c Chain, i int, req RunOptions, res Result, err error) {
	trip, _ := ctx.Value(failFastKey{}).(func())
	if trip == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if i+1 < len(c.Dispatches) && (i != 0 || !ExplicitBackend(req)) && dispatchUnavailable(c.Dispatches[i].Agent, res, err) {
		return
	}
	trip()
}

// Limiter is the batch runner's handle on the dispatch limiter; Slot is a prepared task's claim on a global slot.
type (
	Limiter = limiter
	Slot    = slot
)

// NewBatchLimiter is a limiter for a batch of jobs, with the shared limiter's caps read once.
func NewBatchLimiter(jobs int) *Limiter {
	l := newLimiter(jobs, DefaultPerKey)
	l.limits = router.CurrentLimits()
	return l
}

// Reserve waits for a global slot no prepared task is owed; the claim ends at Settle.
func (l *Limiter) Reserve(ctx context.Context) (*Slot, error) { return l.reserve(ctx) }

// WithBatchSlot carries l and s in ctx so Settle and the dispatches under it find them.
func WithBatchSlot(ctx context.Context, l *Limiter, s *Slot) context.Context {
	return context.WithValue(withLimiter(ctx, l), slotKey{}, s)
}

// WithBatchRun is ctx for one batch task's dispatches: host-isolated and under l.
func WithBatchRun(ctx context.Context, l *Limiter) context.Context {
	return withLimiter(withIsolation(ctx), l)
}

// WithFailFast makes a task that ends without a fallback call stop.
func WithFailFast(ctx context.Context, stop func()) context.Context {
	return context.WithValue(ctx, failFastKey{}, stop)
}

// Settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func Settle(ctx context.Context) { settle(ctx) }
