# Go Concurrency and Performance

Goroutines, channels, `sync`, `context`, then profiling, allocation, layout and runtime tuning (Go 1.27).

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
| Cache with disjoint or write-once keys | `sync.Map` | `sync.Map` as a general map |
| Reuse short-lived hot-path allocations | `sync.Pool` ([below](#syncpool)) | a pool for long-lived or rare objects |
| Broadcast "state changed" | `close(ch)` once, or `sync.Cond` when re-armed | per-waiter sends |
| Collapse identical concurrent lookups | `singleflight.Group` (`x/sync`) | N callers each hitting the DB |
| Sequential lazy transform | an `iter.Seq` chain (1.23+) | a goroutine + channel per stage |

## Goroutine lifetime rules

- Every `go` has a known exit (closed input, cancelled `ctx`, finished job) and a waiter in the starting function.
- No goroutines in library constructors or `init`; expose a blocking `Run(ctx)` and let the caller choose `go`.
- A panic in a spawned goroutine kills the process; recovery: [errors-testing.md](errors-testing.md) → Panics.
- `go 1.22`+ loop variables are per-iteration: drop `v := v` and `tt := tt`.
- Classic leak: a goroutine blocks on a send nobody receives after the caller timed out. Buffer of 1 for a single
  result, or `select` on `ctx.Done()` alongside the send.
- Leak detection: [errors-testing.md](errors-testing.md); live: `runtime.NumGoroutine()`,
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

## Channels and select

- Panics: send on a closed channel, `close` of a nil or closed channel. A nil channel blocks forever on send and
  receive; a closed one receives the zero value with `ok == false`.
- With multiple senders, close from the coordinator after `wg.Wait()`. Closing is a broadcast, not cleanup.
- Buffer size is a design choice: 0 = synchronous handoff, 1 = don't block on a single result, n = a known batch or
  semaphore size (comment why). A large buffer "fixing" a deadlock only hides it.
- A `nil` channel case never fires: set a drained input to `nil` to drop it from a merge loop.
- `default` makes `select` non-blocking; in a `for` loop it busy-spins, so only for try-send/try-receive.
- Ready cases are chosen at random; to prefer `ctx.Done()`, check `ctx.Err()` before the select.
- With `go 1.23`+: `time.After` in a loop no longer leaks and `Stop`/`Reset` deliver no stale tick, so the
  drain-before-reset dance is dead code (older modules still drain when `Stop` returns false). Reuse one
  `time.NewTimer` on a hot loop.

## sync and atomic gotchas

- Never copy a `Mutex`, `WaitGroup`, `Once`, `Cond` or typed atomic after first use: use pointers; `go vet`
  copylocks catches most.
- A plain `map` read and written from two goroutines is a fatal error `recover` can't catch: mutex or `sync.Map`.
- Embedding `sync.Mutex` in an exported struct leaks `Lock`/`Unlock`; use an unexported `mu` field.
- No I/O, channel ops or callbacks inside a critical section.
- `RWMutex` pays only when reads dominate and the section is non-trivial. A recursive `RLock` deadlocks when a writer
  waits between calls; no upgrade (`Lock` while holding `RLock` deadlocks): release, lock, re-check.
- No reentrant mutex: a locking method calling another locking method deadlocks; split into `fooLocked()` helpers.
- `wg.Add` before `go`, never inside the goroutine. `wg.Go(f)` (1.25+) propagates no error and cancels no sibling:
  use `errgroup` for those.
- Typed atomics over `atomic.AddInt64(&x, …)`: alignment handled, plain reads won't compile.
- Atomics make one word consistent, not an invariant across fields: mutex, or one `atomic.Pointer` to an immutable
  struct (copy-on-write).
- `sync.Map`: write-once/read-many or disjoint keys only; no `Len`, `Range` is not a snapshot; for either, or
  overlapping writes, use `map` + `RWMutex`.
- `sync.OnceFunc`/`OnceValue` re-panic on every call if `f` panicked, where `sync.Once` swallows the retry.
- `-race` mechanics: [errors-testing.md](errors-testing.md).

## Pipelines and worker pools

- Each stage creates its output channel and `defer close(out)`s it; every send selects on `ctx.Done()` so an early
  consumer unwinds upstream stages.
- Fan-in: one `WaitGroup` over the workers, one goroutine that `Wait`s then closes the merged output.
- Ordered results: carry the index and write `out[i] = r` into a pre-sized slice (no lock needed).
- Prefer `errgroup.SetLimit` to a hand-rolled pool; own pools only for per-worker state, custom priority/backpressure,
  or draining on shutdown. Channel semaphore: acquire before `go` so the loop blocks.
- Size to the bottleneck: ~`GOMAXPROCS` for CPU-bound, the downstream limit (DB pool, API quota) for I/O-bound.

## context rules

- Never store a ctx in a struct (exception: a struct that is itself one request's lifetime).
- `context.Background()` mid-chain drops deadline and cancellation; never pass `nil` (`context.TODO()` while plumbing);
  tests use `t.Context()` (1.24+).
- Always `defer cancel()` from `WithCancel`/`WithTimeout`/`WithDeadline` (`go vet` lostcancel); exception: a
  constructor returning ctx and `cancel` together.
- A derived context can only shorten the parent's deadline; to outlive the parent, detach.
- Check cancellation where work loops or blocks (`select` on `ctx.Done()`, or `ctx.Err()` per CPU-loop iteration);
  functions calling only ctx-aware APIs need none.
- Don't log a cancellation as a failure. Matching: [errors-testing.md](errors-testing.md).

### Cancellation tools (1.21+)

| API | Use when |
| --- | --- |
| `WithCancelCause`, `WithTimeoutCause` + `Cause(ctx)` | callers need *why* it stopped |
| `WithoutCancel(ctx)` | work outlives the request (audit write, cache fill) but keeps values; add a timeout |
| `AfterFunc(ctx, f)` | cleanup on cancel without a parked goroutine; `f` runs in its own goroutine; call `stop` on success |
| `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` | root context stopping on SIGINT/SIGTERM |

### context in HTTP services

- `r.Context()` cancels on client disconnect, HTTP/2 stream reset or handler return; pass it to every DB/RPC call
  (`QueryContext`, `ExecContext`, never the ctx-less variants). `ctx.Err() != nil` on error: client left, skip the
  response.
- Goroutines outliving the handler must not use `r.Context()`: `WithoutCancel`.
- Outbound: `http.NewRequestWithContext(ctx, …)`; `http.Client.Timeout` is a whole-request cap, ctx the per-call
  budget; set both on a client you own. Per-request budgets live in middleware (`http.TimeoutHandler`), not handlers.
- Graceful shutdown: on the signal context's `Done`, `srv.Shutdown(shutdownCtx)` with its own timeout; `BaseContext`
  ties request contexts to the server. Streams (SSE, websockets) select on `r.Context().Done()` in the write loop.

### context values

- Only request-scoped metadata crossing API boundaries (trace/span ID, auth principal, request ID), never optional
  parameters, config, DB handles, feature flags. A logger only when enriched with request fields.
- Keys: unexported type per package (`type ctxKey struct{}`) with typed `WithX(ctx, v)` / `XFrom(ctx) (v, ok)`.
- Lookup walks a linked list: fine for a handful, not per-item data in a loop.
- Values must be immutable or concurrency-safe.
- `WithoutCancel` and `AfterFunc` keep values, so prefer them over `context.Background()` for detached work.

## Measure first

- Rule out external waits first: many goroutines blocked in `net.(*conn).Read` or `database/sql`, or a trace span
  dominated by an upstream, means fix the query, pool or cache.
- Pick one metric (p99, throughput, RSS, CPU) and a target; one change per measurement; compare with `benchstat`
  over `-count` ≥ 6. Benchmarks: [errors-testing.md](errors-testing.md).
- Put the trimmed `benchstat` delta (with the `goos/goarch/cpu` header) in the `perf(scope):` commit body and a
  one-line comment on the optimized code.

| Question | Tool | Look at |
| --- | --- | --- |
| Where does CPU go? | `go test -cpuprofile` · `/debug/pprof/profile?seconds=30` | `top -cum`, flame graph |
| Who allocates most? | `pprof -sample_index=alloc_objects` (`alloc_space` for bytes) | hot handlers, encoders |
| What holds live memory? | `pprof -sample_index=inuse_space` | large live objects that should be gone |
| Why is it waiting? | block, mutex, goroutine profiles · `fgprof` (on- and off-CPU) | lock and channel waits, I/O |
| Scheduling, GC, latency | `go tool trace` · `trace.FlightRecorder` (1.25+, last N seconds) | STW, assist, idle P |
| Does it escape? Inlined? | `go build -gcflags='-m=2' ./pkg 2>&1` | `escapes to heap`, `cannot inline` |
| How often does GC run? | `GODEBUG=gctrace=1` · `runtime/metrics` | GC CPU %, heap goal, cycles/s |

- `pprof -diff_base=old.pprof new.pprof` compares profiles; `pprof.Do(ctx, labels, fn)` splits one handler's samples
  from another's.
- `runtime.mallocgc` and `gcBgMarkWorker` on top: cut allocations first, tune the runtime after.

## Allocations and escape analysis

- A value escapes when its address is returned, stored in a heap object or interface, captured by an escaping closure
  (a `go` statement), or its size is unknown or too large for the stack.
- In `-m=2`, `leaking param: p` means the caller's argument escapes too; fix at the API: accept a destination
  (`func Fill(dst *T)`, `AppendX(dst []byte, …) []byte`).
- Converting a non-pointer value to `any` can allocate (not small integers or zero-size values). `fmt.*`, `slog.Any`
  and `[]any` parameters in a hot loop allocate per call; use typed code or generics.
- `strconv.AppendInt`/`AppendQuote`, `time.AppendFormat`, `fmt.Appendf` into a reused buffer.
- Hot validation paths: sentinel errors; `fmt.Errorf` allocates each call.
- Go 1.25+ can stack-allocate a small variable-sized `make([]T, n)`; verify with `-m`.
- Reflection (`reflect.Value`, `reflect.DeepEqual`) costs 10-100x typed code: `slices.Equal`, `maps.Equal`,
  `bytes.Equal`, a type switch over chained assertions.

## Preallocate and reuse

- Same-length output: `make([]T, len(in))` with index assignment beats append; keep append for filtering or when
  early return must leave nothing half-filled.
- `make(map[K]V, n)` when the count is known.
- `for k, v := range m`; `for k := range m { m[k] }` hashes twice.
- Compile `regexp` once at package level. Concurrent misses on one key: `singleflight`.

## Strings and bytes

| Need | Use | Why |
| --- | --- | --- |
| Build a string in a loop | `strings.Builder` + `Grow(n)` | `String()` hands over bytes without copying |
| Read and write a buffer, `io` plumbing | `bytes.Buffer` | `io.Reader` and `io.Writer`; `String()` copies |
| Join a few fixed parts | `a + b + c` | one allocation for the expression |
| `+=` in a loop | never | O(n²) copying |

- Copying a `strings.Builder` after its first write panics: pass `*strings.Builder`.
- No allocation for `m[string(b)]` lookups, `string(b) == s`, or ranging over `[]byte(s)`; elsewhere `string`↔`[]byte`
  copies: convert once.
- A substring or subslice keeps the whole parent alive: `strings.Clone` / `bytes.Clone` to keep 8 bytes of 1 MiB.
- `unsafe.String`/`unsafe.StringData` only when the bytes are never modified afterwards.
- `unique.Make` (1.23+) interns strings; handle comparison is one pointer compare.

## sync.Pool

- For short-lived, same-shape objects allocated at a high rate (encode buffers, per-request scratch); for rare or
  long-lived ones the pool costs more than it saves.
- Objects are dropped over two GC cycles (victim cache): not a cache or live-object limit. `Get` may return anything
  (`New` runs only on an empty pool).
- Store pointers: `Put(&buf)`, not `Put(buf)` (slice header to `any` allocates; staticcheck SA6002).
- Reset before `Put`: truncate and clear references to other objects.
- Discard grown objects (`if cap(*bp) > 64<<10 { return }`) so one huge request doesn't pin memory.
- Never return pooled memory to the caller; copy out first.

- Pattern: `bp := pool.Get().(*[]byte)`, build into `(*bp)[:0]`, `bytes.Clone` the result for the caller, then
  `*bp = buf; pool.Put(bp)` only if `cap(buf) <= 64<<10`.

## Slices: internals and gotchas

- Growth doubles while small, then ~1.25× (switch near 256 elements), rounded to a malloc size class; exact sizes
  change between releases, so never depend on when reallocation happens.
- `append` into a subslice with spare capacity overwrites the parent's later elements: hand out `s[lo:hi:hi]` or
  `slices.Clone` when the receiver may append.
- `slices.Delete`, `Compact`, `DeleteFunc` (1.22+) zero the vacated tail; hand-written `s = s[:n]` keeps removed
  pointers reachable.
- `slices.Clip` drops spare capacity but not the array; only a copy shrinks it.
- An array (`[32]byte`) is comparable and a valid map key; pass large ones by pointer.
- Matrices: one allocation (`make([]float64, rows*cols)`), rows as subslices, walked row-major.

## Maps: internals and gotchas

- Go 1.24+ maps are Swiss tables: 8-slot groups with a control word of hash fingerprints (one probe checks 8
  slots), in tables of ≤1024 slots that grow independently, so no long rehash pause.
- Maps never shrink: `delete` and `clear(m)` keep the tables. To free memory, copy into `make(map[K]V, len(old))`.
- Reads copy the value: for large, frequently changing values use `map[K]*V` (one heap object per entry, more to
  scan); small or mostly-read values stay inline.
- `slices.Contains` in a loop is O(n·m): build a `map[T]struct{}` once.
- Pointer-free keys and values (ints, fixed arrays) spare the GC from scanning the map; matters at millions of entries.

## Struct layout and locality

- Fields pad to alignment in declared order: largest to smallest (`bool,int64,bool,int32` is 24 bytes, reordered 16);
  `fieldalignment` (`golang.org/x/tools`) reports them.

- A trailing zero-size field gets padded (`struct{ v int64; _ struct{} }` is 16 bytes); put it first.
- Reorder only structs allocated by the million or iterated in hot loops; elsewhere group by meaning.
- A loop reading one or two fields: struct of arrays (`xs, ys []float64`) beats array of structs.
- Nodes in a `[]Node` linked by `int32` indices stay contiguous and need no GC scan.
- False sharing: counters written by different cores on one 64-byte line; pad (`_ [56]byte`) only when a benchmark
  shows more goroutines slowing it.
- `container/list` loses to a slice nearly always (allocation and cache miss per node); keep it for O(1) removal via
  a held handle (LRU). Priority queues: `container/heap`.

## CPU micro-patterns

- Inlining: look for `cannot inline … function too complex` on hot leaf functions; move logging and metrics out of
  small helpers.
- Bounds checks: index the last element first (`_ = s[3]`) or `range s`; confirm with
  `-gcflags='-d=ssa/check_bce'`.
- One-accumulator sums form a dependency chain: use 2-4 accumulators when a profile blames the loop.
- `math/bits` compiles to single instructions (popcount, clz, rotate). Assembly or `GOEXPERIMENT=simd` only for a
  profiled numeric kernel, with a pure-Go fallback.
- A cgo call costs tens of ns and pins the OS thread: batch across the boundary.
- A disabled `slog` line still formats its arguments: `slog.LogAttrs` with typed attributes avoids the allocation.

## I/O

- Check `bufio.Writer.Flush`'s error; raise `bufio.Scanner`'s 64 KiB token limit with `Buffer`; stream with
  `io.Copy` or `json.Decoder`, not `io.ReadAll` on unbounded input.
- `http.Transport` keeps 2 idle connections per host by default (`MaxIdleConnsPerHost`): raise it to your
  concurrency, share one client, read the body to EOF before `Close`.
- Batch round-trips (multi-row insert, `COPY`, bulk APIs); per-call overhead usually beats payload savings.

## Runtime tuning

| Knob | Effect | Use when |
| --- | --- | --- |
| `GOGC` (default 100) | next GC when the heap grows by GOGC% over live heap | trading memory for fewer cycles |
| `GOMEMLIMIT` | soft limit on total Go memory; GC runs harder near it | container limits, avoiding OOM |
| `GOGC=off` + `GOMEMLIMIT` | GC runs only near the limit | batch jobs with a known, stable working set |
| `GOMAXPROCS` | number of Ps; follows the cgroup CPU limit and its changes (1.25+) | rarely set by hand now |
| `default.pgo` | profile-guided inlining and devirtualization | long-running services with a stable profile |

- `GOMEMLIMIT` ≈ 80–90% of the container limit (stacks, cgo allocations and mmap count toward the container, not the
  Go heap). It is soft: GC caps itself at ~50% CPU, so a live heap above the limit still OOMs.
- Runtime setters: `debug.SetGCPercent`, `debug.SetMemoryLimit`. Memory ballast is obsolete; delete it.
- Go 1.26+ uses the Green Tea collector: re-measure GC CPU instead of reusing old tuning.
- A finished goroutine keeps its large stack until GC shrinks it (deep recursion in pooled workers: high `StackInuse`).
- PGO: save a 30–60 s CPU profile of representative production load as `default.pgo` in the main package directory;
  `go build` picks it up (`-pgo=auto`). Expect a few percent, mostly devirtualized interface calls. Merge with
  `go tool pprof -proto a.pprof b.pprof > default.pgo`; refresh after large refactors (stale helps less, no harm).
- `weak.Pointer[T]` and `runtime.AddCleanup` (1.24+) build GC-reclaimable caches and replace `SetFinalizer`; cleanups
  never resurrect the object, and one object can have several.
