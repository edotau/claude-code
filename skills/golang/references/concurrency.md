# Go Concurrency

Goroutine lifetimes, channels, `select`, `sync`/`atomic`, pipelines and `context` cancellation for Go 1.22+.

## Decide the primitive first

| Need | Reach for | Not |
| --- | --- | --- |
| Hand ownership of data to another goroutine | unbuffered/buffered channel | shared slice + mutex |
| Guard a few fields read and written in place | `sync.Mutex` next to the fields | a channel used as a lock |
| Counter, flag, or swapped config pointer | `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]` | mutex around one int |
| Lazy one-time init returning a value | `sync.OnceValue` / `sync.OnceValues` | hand-rolled `sync.Once` + package var |
| Run N tasks, first error cancels the rest | `errgroup.WithContext` (`x/sync`) | `WaitGroup` + error channel |
| Wait for N tasks, no errors | `sync.WaitGroup` (`wg.Go(f)` on 1.25+) | sleeping, polling |
| Bound parallelism | `g.SetLimit(n)` or a `chan struct{}` semaphore of size n | spawning unbounded goroutines |
| Cache with disjoint or write-once keys | `sync.Map` | `sync.Map` as a general map (slower on mixed writes) |
| Reuse short-lived allocations on a hot path | `sync.Pool` | a pool for long-lived or rarely used objects |
| Broadcast "state changed" to many waiters | `close(ch)` once, or `sync.Cond` when re-armed | per-waiter sends |
| Collapse identical concurrent lookups (stampede) | `singleflight.Group` (`x/sync`) | N callers each hitting the DB |
| Sequential lazy transform, no parallelism needed | an `iter.Seq` chain (1.23+) | a goroutine + channel per stage |

First ask whether the work needs concurrency at all: a goroutine without a measured win is only a new failure mode.

## Goroutine lifetime rules

- Every `go` statement has a known exit — a closed input, a cancelled `ctx`, or a finished bounded job; write down
  which before you write the goroutine. A goroutine with no exit is a leak, and leaks grow with request count.
- The function that starts a goroutine waits for it (`WaitGroup`, `errgroup`, done channel) — so callers can't
  return while work still touches their data.
- Don't start goroutines in library constructors or `init`; expose a `Run(ctx)` that blocks and let the caller choose
  `go` — concurrency stays visible at the call site and shutdown composes.
- A panic in any goroutine kills the process; `recover` works only in the goroutine that panicked, so a server
  that must survive bad input defers its own recover inside each worker it spawns.
- Go 1.22 loop variables are per-iteration once `go.mod` says `go 1.22`+: `for _, v := range xs { go func() { use(v)
  }() }` is then correct, so drop the `v := v` shadow. `for i := range n` replaces `for i := 0; i < n; i++`.
- Classic leak: a goroutine blocks on a send nobody receives after the caller timed out. Fix with a buffer of 1
  for a single result, or `select` on `ctx.Done()` alongside the send.
- Catch leaks in tests with `goleak.VerifyTestMain(m)` only if the module already imports it
  ([testing.md](testing.md)); in a live process, compare `runtime.NumGoroutine()` over time and read
  `/debug/pprof/goroutine?debug=2` stacks.

```go
func first(ctx context.Context, q string) (Result, error) {
    ch := make(chan Result, 1) // buffer 1: the sender never blocks if we return early
    go func() { ch <- search(q) }()
    select {
    case r := <-ch:
        return r, nil
    case <-ctx.Done():
        return Result{}, context.Cause(ctx)
    }
}
```

## Channels

| Operation | nil channel | open channel | closed channel |
| --- | --- | --- | --- |
| send | blocks forever | blocks until received / buffer slot | **panics** |
| receive | blocks forever | blocks until value | zero value, `ok == false`, never blocks |
| `close` | **panics** | succeeds | **panics** |

- Only the sender closes, and only when it's the sole sender. With multiple senders, close from the coordinator
  after `wg.Wait()` — closing from a sender races the others into a send-on-closed panic.
- Closing is a broadcast, not a cleanup: you never need to close a channel only to free it; the GC reclaims it.
- Use direction in signatures (`<-chan T`, `chan<- T`) — the compiler then rejects a consumer that closes.
- Buffer size is a design choice, not a tuning knob: 0 (the default) = synchronous handoff, 1 = "don't block the
  producer on a single result", n = a known batch or semaphore size — comment why n and what happens when it fills.
  A large buffer to "fix" a deadlock only hides it and masks backpressure.
- Send values or immutable data, not pointers to state the sender keeps touching — a shared pointer on a channel
  is shared memory without a lock, and the race detector is the only thing that will notice.
- `chan struct{}` for signals — zero-size elements, and the intent is unambiguous.
- `for v := range ch` ends only on close; a forgotten close hangs the consumer.

## select

- A `nil` channel case never fires — set a drained input to `nil` to drop it from a merge loop without restructuring.
- `default` makes the select non-blocking; inside a `for` loop it busy-spins, so use it only for try-send or
  try-receive.
- When several cases are ready the choice is random — never rely on case order for priority. To prefer `ctx.Done()`,
  check `ctx.Err()` before the select.
- Go 1.23+ timers (with `go.mod` at `go 1.23`+): `time.After` in a loop no longer leaks, and `Stop`/`Reset` no
  longer deliver a stale tick, so the drain-before-reset dance is dead code; older modules still drain when `Stop`
  returns false. Still prefer one `time.NewTimer` reused across iterations on a
  hot loop to avoid an allocation per pass.

```go
for a != nil || b != nil {
    select {
    case v, ok := <-a:
        if !ok { a = nil; continue }
        out <- v
    case v, ok := <-b:
        if !ok { b = nil; continue }
        out <- v
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

## sync and atomic gotchas

- Never copy a `Mutex`, `WaitGroup`, `Once`, `Cond` or typed atomic after first use — pass pointers, and use
  pointer receivers on structs that embed them; `go vet` (copylocks) catches most cases.
- A plain `map` read and written from two goroutines is a fatal runtime error, not a recoverable panic — guard it
  with a mutex or use `sync.Map`; `recover` can't save the process.
- Name the mutex `mu`, unexported — embedding `sync.Mutex` in an exported struct leaks `Lock`/`Unlock` into its API.
- Keep the mutex on the line above the fields it guards, and keep critical sections free of I/O, channel ops and
  callbacks — any of those under a lock invites deadlock and latency spikes.
- `defer mu.Unlock()` right after `Lock()` unless the hot path has been measured; an early return then can't leak
  the lock.
- `RWMutex` pays only when reads dominate and the critical section is non-trivial; otherwise plain `Mutex` is faster.
  A recursive `RLock` deadlocks when a writer is waiting between the two calls, and there is no upgrade: taking
  `Lock` while holding `RLock` deadlocks — release, lock, then re-check the condition.
- Go has no reentrant mutex: a method holding the lock that calls another locking method deadlocks — split into
  `fooLocked()` helpers that assume the lock is held.
- `wg.Add` before `go`, never inside the goroutine — otherwise `Wait` can return before `Add` runs.
- Typed atomics (`atomic.Int64`, `atomic.Pointer[T]`) over the `atomic.AddInt64(&x, …)` functions: alignment is
  handled and plain reads of the field won't compile.
- Atomics make one word consistent, not an invariant across two fields; two related values need a mutex or a
  single `atomic.Pointer` to an immutable struct (copy-on-write).
- `sync.Pool`: `Reset` objects before `Put`, never assume `Get` hands back a zeroed or surviving object (`New` runs
  only on an empty pool; GC empties pools), and drop oversized buffers or one huge request pins memory forever.
- `sync.Map` has no `Len`, and `Range` is not a snapshot; when you need either, or writes overlap on the same keys,
  use a `map` + `RWMutex`.
- `wg.Go(f)` (1.25+) does the `Add`/`Done` bookkeeping but still propagates no error and cancels no sibling —
  anything that returns an error or must stop the others goes through `errgroup`.
- `sync.OnceFunc`/`OnceValue` re-panic on every call if `f` panicked, where `sync.Once` swallows the retry — prefer
  them for init that can fail loudly.
- Run tests with `-race` on every CI build; a race report is a bug even when the output "looks right". Test-file
  conventions: [testing.md](../../../rules/software/go/testing.md).

## Pipelines and worker pools

- Each stage owns its output channel: it creates it, writes it, and `defer close(out)`s it. Downstream ranges until
  close.
- Every send in a stage selects on `ctx.Done()` — a consumer that stops early must be able to unwind all
  upstream stages, or they leak blocked on sends.
- Fan-out: n workers read one input channel. Fan-in: one `WaitGroup` over the workers, and a single goroutine that
  `Wait`s then closes the merged output.
- Results whose order matters: carry the index with the value and write into a pre-sized slice
  (`out[i] = r`; distinct indices need no lock) instead of sorting afterwards.
- Prefer `errgroup.SetLimit` to a hand-rolled pool; write your own only for per-worker state (a connection, a
  buffer), custom priority/backpressure, or draining in-flight tasks on shutdown. With a channel semaphore, acquire
  before `go` so the loop itself blocks instead of parking thousands of goroutines.
- Stages that are sequential and CPU-light (filter, map over a slice or cursor) are an `iter.Seq` chain, not a
  pipeline — goroutines pay off only for I/O or true multi-core parallelism.
- Size the pool to the bottleneck: ~`GOMAXPROCS` for CPU-bound work, the downstream limit (DB pool, API quota) for
  I/O-bound — never "one goroutine per item" over unbounded input.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(8)
out := make([]Result, len(items))
for i, it := range items {
    g.Go(func() error {
        r, err := fetch(ctx, it)
        out[i] = r
        return err
    })
}
if err := g.Wait(); err != nil { return nil, err }
```

## context: rules

- `ctx context.Context` is the first parameter of anything that blocks, does I/O, or fans out; never store it in
  a struct (exception: a struct that is itself one request's lifetime) — stored contexts outlive their cancellation.
- Propagate the caller's ctx through every layer; a `context.Background()` mid-call-chain silently drops the
  deadline and cancellation, so the work keeps running after the client is gone.
- Never pass `nil` (it panics on the first `Done`/`Value`, far from the caller); use `context.TODO()` while plumbing
  is incomplete and `context.Background()` only at the top of `main`, a test, or a truly detached root. In tests use
  `t.Context()` (1.24+).
- Always `defer cancel()` from `WithCancel`/`WithTimeout`/`WithDeadline` — the timer and child links leak until
  the parent ends otherwise; `go vet` (lostcancel) flags the misses. The one exception: a constructor that returns
  the ctx and its `cancel` together, handing the obligation to the caller.
- A derived context can only shorten the parent's deadline, never extend it; to outlive the parent, detach.
- Check cancellation where work loops or blocks: `select` on `ctx.Done()`, or `if err := ctx.Err(); err != nil`
  between iterations of a CPU loop. Functions that only call ctx-aware APIs need no extra check.
- Compare with `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded` — wrapped errors break `==`.
- Return `ctx.Err()` (or `context.Cause(ctx)`) as-is or wrapped with `%w`; don't log a cancellation as a failure —
  it's usually the caller leaving.

## context: cancellation tools (1.21+)

| API | Use when |
| --- | --- |
| `WithCancelCause`, `WithTimeoutCause` + `Cause(ctx)` | callers need *why* it stopped, not just `Canceled` |
| `WithoutCancel(ctx)` | work outlives the request (audit write, cache fill) but keeps its values; add a timeout |
| `AfterFunc(ctx, f)` | cleanup on cancel without a parked goroutine; `f` runs in its own goroutine; `stop` on success |
| `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` | root context that stops on SIGINT/SIGTERM |

```go
bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
go func() { defer cancel(); audit.Write(bg, ev) }()
```

- `AfterFunc` is the clean way to bridge a ctx to an API that only knows `Close`/`SetDeadline` (a `net.Conn`, a
  blocking read): `stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })`.

## context in HTTP services

- `r.Context()` cancels when the client disconnects, the HTTP/2 stream resets, or the handler returns — pass it
  to every DB/RPC call (`QueryContext`, `ExecContext`, `QueryRowContext` — never the ctx-less variants) so
  abandoned requests stop costing work. On an error with `ctx.Err() != nil` the client left: skip the response.
- Goroutines spawned from a handler that outlive it must not use `r.Context()` (it's cancelled on return) — detach
  with `WithoutCancel` as above.
- Outbound calls: `http.NewRequestWithContext(ctx, …)`; `http.Client.Timeout` is a whole-request cap, ctx is the
  per-call budget — set both on a client you own.
- Per-request budgets go in middleware (`http.TimeoutHandler` or `WithTimeout` on the request context), not in each
  handler.
- Graceful shutdown: on the signal context's `Done`, call `srv.Shutdown(shutdownCtx)` with its own timeout; it stops
  accepting and waits for in-flight handlers. Set `BaseContext` to tie request contexts to the server's lifetime.
- Long-lived streams (SSE, websockets) select on `r.Context().Done()` in the write loop — that's the only signal a
  client left.

## context values

- Values carry request-scoped metadata that crosses API boundaries — trace/span IDs, auth principal, request ID —
  never optional parameters, config, DB handles, feature flags, or anything a function needs to be correct. A logger
  belongs there only when it is enriched with request fields (trace ID); otherwise inject it.
- Keys are an unexported type per package (`type ctxKey struct{}`) so no other package can collide or read them
  directly; expose typed `WithX(ctx, v)` / `XFrom(ctx) (v, ok)` accessors.
- Lookup is a linked-list walk up the chain — cheap for a handful of values, so don't stash per-item data in a loop.
- Values must be immutable or safe for concurrent use: every goroutine holding the ctx can read them.
- `WithoutCancel` and `AfterFunc` keep values, so trace IDs survive into detached work — the reason to prefer them
  over `context.Background()`.

## Review checklist

- Each `go` has a stated exit and a waiter; each channel has one closer; each `WithX` has a `defer cancel()`.
- No lock held across I/O, a channel op, or a callback; no copied `sync` values (`go vet ./...` clean).
- Every blocking send/receive in long-lived code selects on `ctx.Done()`; no `context.Background()` below the root.
- No map shared across goroutines without a lock; no pointer to live state sent on a channel.
- Unbounded input fans out through a limit, not one goroutine per item.
- `-race` ran on the tests that exercise the change.

Adapted from samber/cc-skills-golang (MIT): golang-concurrency, golang-context.
