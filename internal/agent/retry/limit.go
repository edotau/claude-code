package retry

import (
	"context"
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

// SharedTryClaim is the machine-wide limiter seam; tests count or stub it.
var SharedTryClaim = inflight.TryClaim

// SharedPoll is how often a waiting dispatch re-checks the shared limiter; a var so tests can shorten it.
var SharedPoll = 250 * time.Millisecond

// dispatchSeq makes each permit's shared marker id unique within the process.
var dispatchSeq atomic.Uint64

// Limiter is the one concurrency gate for dispatches: at most jobs run at once, and at most cap per billed account.
// A waiting task holds nothing, so a full account never blocks another.
type Limiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	jobs, cap int
	per       map[string]int
	pending   int             // prepared tasks that have not yet reached their first dispatch: each still owes a slot
	limits    inflight.Limits // shared-limiter caps, read once per batch; zero Total = no shared limiter
}

// Slot is one prepared task's claim on a global slot until its first dispatch asks for a permit (or parks, or ends).
type Slot struct{ settled bool }

// NewLimiter is a limiter running at most jobs dispatches at once and cap per billed account.
func NewLimiter(jobs, cap int) *Limiter {
	l := &Limiter{jobs: jobs, cap: cap, per: map[string]int{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// wait blocks until ok() holds under the lock, or ctx is done; a cancelled ctx wins over a free slot.
func (l *Limiter) wait(ctx context.Context, ok func() bool) error {
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

// Reserve waits for a global slot no prepared task is owed: a task is prepared only once it can start; Settle ends it.
func (l *Limiter) Reserve(ctx context.Context) (*Slot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.wait(ctx, func() bool { return l.running+l.pending < l.jobs }); err != nil {
		return nil, err
	}
	l.pending++
	return &Slot{}, nil
}

func (l *Limiter) settleLocked(s *Slot) {
	if s != nil && !s.settled {
		s.settled = true
		l.pending--
		l.cond.Broadcast()
	}
}

// Settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func Settle(ctx context.Context) {
	if l, _ := ctx.Value(limiterKey{}).(*Limiter); l != nil {
		s, _ := ctx.Value(slotKey{}).(*Slot)
		l.mu.Lock()
		l.settleLocked(s)
		l.mu.Unlock()
	}
}

// acquire waits for a global Slot, an account permit and one shared slot; a full shared account releases the local
// permit before the next try, so it never holds a worker another account could use. The release func is idempotent.
func (l *Limiter) Acquire(ctx context.Context, account string) (func(), error) {
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
		if ok, _ := SharedTryClaim(c, l.limits); ok {
			var once sync.Once
			return func() { once.Do(func() { inflight.Release(c.Owner, c.ID); freeLocal() }) }, nil
		}
		freeLocal()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(SharedPoll):
		}
	}
}

// acquireLocal is the in-process admit: a global slot and, when account is set, an account permit.
func (l *Limiter) acquireLocal(ctx context.Context, account string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, _ := ctx.Value(slotKey{}).(*Slot)
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

// Permit is a dispatch's current permit; Do swaps it out around retry waits so a backing-off dispatch holds nothing.
type Permit struct {
	Account string
	Release func()
	Recheck func() *Error // a skip when the account became unusable since the dispatch started
}

// Idle runs wait with the permit released, then takes a permit again; the dispatch runs a child only while holding one.
func (h *Permit) Idle(ctx context.Context, wait func()) error {
	h.Release()
	wait()
	r, err := AcquireDispatch(ctx, h.Account)
	if err != nil {
		h.Release = func() {}
		return err
	}
	h.Release = r
	return nil
}

type (
	permitKey   struct{}
	limiterKey  struct{}
	failFastKey struct{}
	slotKey     struct{}
)

// WithPermit carries h in ctx for the dispatch's retry loop; Held reads it back (nil outside a dispatch).
func WithPermit(ctx context.Context, h *Permit) context.Context {
	return context.WithValue(ctx, permitKey{}, h)
}

func Held(ctx context.Context) *Permit {
	h, _ := ctx.Value(permitKey{}).(*Permit)
	return h
}

// InBatch reports whether ctx runs under a batch limiter.
func InBatch(ctx context.Context) bool { return ctx.Value(limiterKey{}) != nil }

// FailFastStop is the stop func WithFailFast stored in ctx, nil outside a fail-fast batch.
func FailFastStop(ctx context.Context) func() {
	stop, _ := ctx.Value(failFastKey{}).(func())
	return stop
}

func WithLimiter(ctx context.Context, l *Limiter) context.Context {
	return context.WithValue(ctx, limiterKey{}, l)
}

// AcquireDispatch takes a dispatch permit under ctx's limiter; outside a batch there is no limit.
func AcquireDispatch(ctx context.Context, account string) (release func(), err error) {
	l, _ := ctx.Value(limiterKey{}).(*Limiter)
	if l == nil {
		return func() {}, nil
	}
	return l.Acquire(ctx, account)
}

// NewBatchLimiter is a limiter for a batch of jobs, with the shared limiter's caps read once.
func NewBatchLimiter(jobs int) *Limiter {
	l := NewLimiter(jobs, DefaultPerKey)
	l.limits = router.CurrentLimits()
	return l
}

// WithBatchSlot carries l and s in ctx so Settle and the dispatches under it find them.
func WithBatchSlot(ctx context.Context, l *Limiter, s *Slot) context.Context {
	return context.WithValue(WithLimiter(ctx, l), slotKey{}, s)
}

// WithFailFast makes a task that ends without a fallback call stop.
func WithFailFast(ctx context.Context, stop func()) context.Context {
	return context.WithValue(ctx, failFastKey{}, stop)
}
