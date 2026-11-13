# Go Concurrency

Goroutine lifetimes, channels, `select`, `sync`/`atomic`, pipelines and `context` cancellation for Go 1.22+.

## Decide the primitive first

| Need | Reach for | Not |
| --- | --- | --- |
| Hand ownership of data to another goroutine | channel | shared slice + mutex |
| Guard a few fields read and written in place | `sync.Mutex` next to the fields | a channel used as a lock |
| Counter, flag, or swapped config pointer | `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]` | mutex around one int |
| Lazy one-time init returning a value | `sync.OnceValue` / `sync.OnceValues` | `sync.Once` + package var |
| N tasks, first error cancels the rest | `errgroup.WithContext` (`x/sync`) | `WaitGroup` + error channel |
| Wait for N tasks, no errors | `sync.WaitGroup` (`wg.Go(f)` on 1.25+) | sleeping, polling |
| Bound parallelism | `g.SetLimit(n)` or a `chan struct{}` semaphore | unbounded goroutines |
| Cache with disjoint or write-once keys | `sync.Map` | `sync.Map` as a general map (slower on mixed writes) |
| Reuse short-lived hot-path allocations | `sync.Pool` ([performance.md](performance.md)) | a pool for long-lived or rare objects |
| Broadcast "state changed" | `close(ch)` once, or `sync.Cond` when re-armed | per-waiter sends |
| Collapse identical concurrent lookups | `singleflight.Group` (`x/sync`) | N callers each hitting the DB |
| Sequential lazy transform | an `iter.Seq` chain (1.23+) | a goroutine + channel per stage |

## Goroutine lifetime rules

- Every `go` has a known exit (closed input, cancelled `ctx`, finished job) and a waiter in the starting function.
- No goroutines in library constructors or `init`; expose a blocking `Run(ctx)` and let the caller choose `go`.
- A panic in a spawned goroutine kills the process; recovery: [errors.md](errors.md) → Panics.
- With `go 1.22`+ in `go.mod` loop variables are per-iteration: drop the `v := v` shadow (and `tt := tt` before
  `t.Parallel()`).
- Classic leak: a goroutine blocks on a send nobody receives after the caller timed out. Buffer of 1 for a single
  result, or `select` on `ctx.Done()` alongside the send.
- Leak detection in tests: [testing.md](testing.md); live: `runtime.NumGoroutine()` over time and
  `/debug/pprof/goroutine?debug=2`.

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

- Panics: send on a closed channel, `close` of a nil or closed channel. A nil channel blocks forever on send and
  receive; a closed one receives the zero value with `ok == false`.
- With multiple senders, close from the coordinator after `wg.Wait()`.
- Closing is a broadcast, not cleanup; the GC reclaims unclosed channels.
- Buffer size is a design choice: 0 = synchronous handoff, 1 = don't block on a single result, n = a known batch or
  semaphore size (comment why). A large buffer "fixing" a deadlock only hides it.
- Send values or immutable data, not pointers to state the sender keeps touching (shared memory without a lock).

## select

- A `nil` channel case never fires: set a drained input to `nil` to drop it from a merge loop.
- `default` makes it non-blocking; in a `for` loop it busy-spins, so only for try-send/try-receive.
- Ready cases are chosen at random; to prefer `ctx.Done()`, check `ctx.Err()` before the select.
- With `go 1.23`+ in `go.mod`: `time.After` in a loop no longer leaks and `Stop`/`Reset` deliver no stale tick, so the
  drain-before-reset dance is dead code (older modules still drain when `Stop` returns false). Reuse one
  `time.NewTimer` on a hot loop.

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

- Never copy a `Mutex`, `WaitGroup`, `Once`, `Cond` or typed atomic after first use: pointers and pointer receivers;
  `go vet` copylocks catches most.
- A plain `map` read and written from two goroutines is a fatal error `recover` can't catch: mutex or `sync.Map`.
- Embedding `sync.Mutex` in an exported struct leaks `Lock`/`Unlock`; use an unexported `mu` field.
- No I/O, channel ops or callbacks inside a critical section.
- `RWMutex` pays only when reads dominate and the section is non-trivial. A recursive `RLock` deadlocks when a writer
  waits between calls; there is no upgrade (`Lock` while holding `RLock` deadlocks): release, lock, re-check.
- No reentrant mutex: a locking method calling another locking method deadlocks; split into `fooLocked()` helpers.
- `wg.Add` before `go`, never inside the goroutine.
- Typed atomics over `atomic.AddInt64(&x, …)`: alignment handled, plain reads won't compile.
- Atomics make one word consistent, not an invariant across fields: use a mutex or one `atomic.Pointer` to an
  immutable struct (copy-on-write).
- `sync.Map` has no `Len` and `Range` is not a snapshot; for either, or overlapping writes, use `map` + `RWMutex`.
- `wg.Go(f)` (1.25+) propagates no error and cancels no sibling: use `errgroup` for those.
- `sync.OnceFunc`/`OnceValue` re-panic on every call if `f` panicked, where `sync.Once` swallows the retry.
- `-race` mechanics: [testing.md](testing.md).

## Pipelines and worker pools

- Each stage creates its output channel and `defer close(out)`s it; every send selects on `ctx.Done()` so an early
  consumer unwinds upstream stages.
- Fan-in: one `WaitGroup` over the workers, one goroutine that `Wait`s then closes the merged output.
- Ordered results: carry the index and write `out[i] = r` into a pre-sized slice (no lock needed).
- Prefer `errgroup.SetLimit` to a hand-rolled pool; own pools only for per-worker state, custom priority/backpressure,
  or draining on shutdown. Channel semaphore: acquire before `go` so the loop blocks.
- Size to the bottleneck: ~`GOMAXPROCS` for CPU-bound, the downstream limit (DB pool, API quota) for I/O-bound.

## context: rules

- Never store a ctx in a struct (exception: a struct that is itself one request's lifetime).
- `context.Background()` mid-chain drops deadline and cancellation.
- Never pass `nil` ctx; `context.TODO()` while plumbing is incomplete. In tests `t.Context()` (1.24+).
- Always `defer cancel()` from `WithCancel`/`WithTimeout`/`WithDeadline` (`go vet` lostcancel); exception: a
  constructor returning ctx and `cancel` together.
- A derived context can only shorten the parent's deadline; to outlive the parent, detach.
- Check cancellation where work loops or blocks (`select` on `ctx.Done()`, or `ctx.Err()` per CPU-loop iteration);
  functions calling only ctx-aware APIs need none.
- Don't log a cancellation as a failure. Matching: [errors.md](errors.md).

## context: cancellation tools (1.21+)

| API | Use when |
| --- | --- |
| `WithCancelCause`, `WithTimeoutCause` + `Cause(ctx)` | callers need *why* it stopped |
| `WithoutCancel(ctx)` | work outlives the request (audit write, cache fill) but keeps values; add a timeout |
| `AfterFunc(ctx, f)` | cleanup on cancel without a parked goroutine; `f` runs in its own goroutine; call `stop` on success |
| `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` | root context stopping on SIGINT/SIGTERM |

```go
bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
go func() { defer cancel(); audit.Write(bg, ev) }()
```

- `AfterFunc` bridges a ctx to an API that only knows `Close`/`SetDeadline`:
  `stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })`.

## context in HTTP services

- `r.Context()` cancels on client disconnect, HTTP/2 stream reset or handler return; pass it to every DB/RPC call
  (`QueryContext`, `ExecContext`, never the ctx-less variants). `ctx.Err() != nil` on error: client left, skip the
  response.
- Goroutines outliving the handler must not use `r.Context()`: `WithoutCancel`.
- Outbound: `http.NewRequestWithContext(ctx, …)`; `http.Client.Timeout` is a whole-request cap, ctx the per-call
  budget; set both on a client you own.
- Per-request budgets in middleware (`http.TimeoutHandler` or `WithTimeout`), not each handler.
- Graceful shutdown: on the signal context's `Done`, `srv.Shutdown(shutdownCtx)` with its own timeout. `BaseContext`
  ties request contexts to the server's lifetime.
- Long-lived streams (SSE, websockets) select on `r.Context().Done()` in the write loop.

## context values

- Only request-scoped metadata crossing API boundaries (trace/span ID, auth principal, request ID) — never optional
  parameters, config, DB handles, feature flags. A logger only when enriched with request fields.
- Keys: unexported type per package (`type ctxKey struct{}`) with typed `WithX(ctx, v)` / `XFrom(ctx) (v, ok)`.
- Lookup walks a linked list: fine for a handful, not per-item data in a loop.
- Values must be immutable or concurrency-safe.
- `WithoutCancel` and `AfterFunc` keep values, so prefer them over `context.Background()` for detached work.

## Review checklist

- Each `go` has an exit and a waiter; each channel one closer; each `WithX` a `defer cancel()`.
- No lock held across I/O, channel op or callback; no copied `sync` values (`go vet ./...` clean).
- Blocking sends/receives in long-lived code select on `ctx.Done()`; no `context.Background()` below the root.
- No shared map without a lock; no pointer to live state on a channel.
- Unbounded input fans out through a limit.
- `-race` ran on the tests exercising the change.
