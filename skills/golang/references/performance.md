# Go Concurrency and Performance

Measurement and test scope: [quality.md](../../../rules/standards/quality.md) and
[testing-guidelines.md](../../../rules/standards/testing-guidelines.md). Benchmark interpretation:
[errors-testing.md](errors-testing.md#benchmarks).

## Goroutine lifetime rules

- Every goroutine has an exit and a waiter. Library constructors expose blocking `Run(ctx)`, not hidden goroutines.
- A timed-out caller can strand a sender: use a buffer of one for a single result, or a cancellable send.
- A parent cannot recover a child's panic: [errors-testing.md](errors-testing.md#panics).
- Prefer `errgroup.SetLimit` to a custom pool unless workers need state, priorities or shutdown draining. Acquire a
  semaphore before spawning; size CPU work to `GOMAXPROCS`, I/O work to the downstream limit.

## Channels and select

- Set drained inputs to nil to remove them from a merge select. A loop with `default` can busy-spin.
- Ready cases are chosen pseudo-randomly; a `ctx.Err()` check before select notices existing cancellation first.
- Close multi-sender output after all senders finish; every pipeline send must allow downstream cancellation.
- With a Go 1.23+ module, unreferenced timers are collected and channel timers have no stale ticks after Stop/Reset;
  older modules need the old drain logic. Reuse a timer when allocation matters.

## sync and atomic gotchas

- A concurrent map access failure is not recoverable. `sync.Map.Range` is not a snapshot; use it for write-once or
  disjoint keys, not overlapping updates or length/snapshot requirements.
- Recursive `RLock` can deadlock behind a waiting writer. Lock upgrades require release, lock, then re-check.
  A locking method calling another locking method also deadlocks; keep locked helpers separate.
- Keep I/O, channel operations and callbacks outside locks. Measure before choosing `RWMutex`.
- Atomics protect one value, not a multi-field invariant; publish an immutable struct through one atomic pointer.
- `WaitGroup.Go` neither propagates errors nor cancels siblings. `OnceFunc`/`OnceValue` repeat a panic on later calls;
  plain `Once` marks the panicking call done and never retries it.

## context rules

- Store context only in a struct representing one request's lifetime. A derived deadline cannot extend its parent's.
- Detached work uses `WithoutCancel` to keep values, with its own timeout. `AfterFunc` runs in a separate goroutine;
  stop its callback on successful completion. HTTP shutdown is in [services.md](services.md#graceful-shutdown).
- Context values are immutable request metadata, not config or dependencies; use a private key type and typed accessors.
  Harness per-run values are owned by [go.md](../../../rules/harness/go.md#style-and-lint).
- Do not log expected cancellation as failure. CPU loops and blocking sends need cancellation checks; callers of
  context-aware APIs need no redundant polling.

## Measure first

Rule out upstream waits before tuning CPU or GC. Compare one metric and one change at a time; keep the machine and
workload comparable. Profiles answer different questions:

| Question | Evidence |
| --- | --- |
| CPU vs waiting | CPU profile vs block/mutex/goroutine profiles or execution trace |
| Allocation churn vs retained heap | `alloc_objects`/`alloc_space` vs `inuse_space` |
| Escapes or failed inlining | `rtk go build -gcflags='-m=2' ./internal/<pkg>` |
| Change from baseline | `pprof -diff_base=old.pprof new.pprof` |

Use `pprof.Do` labels to separate handlers. Allocation/GC hotspots call for fewer allocations before runtime tuning.
Keep the measured delta and platform in the performance report; comments explain the optimization's invariant.

## Allocations and escape analysis

- `leaking param` can push allocation into the caller: consider destination-buffer APIs. Interface boxing and
  formatting in hot loops can allocate; verify compiler output rather than assuming a conversion is free.
- A substring/subslice retains its whole backing allocation; clone when retaining a small part of a large buffer.
- Copying a used `strings.Builder` panics. `unsafe.String` requires backing bytes that stay alive and unmodified.
  Write composition: [style.md](style.md#strings).

## sync.Pool

Use for frequent, short-lived scratch objects, not durable caching or live-object limits: GC may discard entries.
Store pointers to avoid boxing slice headers, clear references on return, and discard oversized buffers. Copy results
out before returning storage to the pool; never expose pooled memory to the caller.

## Slices: internals and gotchas

- Append through a subslice can overwrite its parent's tail: cap it with `s[lo:hi:hi]` or clone before handing it out.
- `slices.Clip` retains the backing array; only a copy releases it. Truncating a pointer slice retains the removed
  objects unless cleared; modern `slices.Delete`/`Compact` clear their vacated tail.
- Never depend on growth ratios or allocation size classes; they change across releases.

## Maps: internals and gotchas

`delete` and `clear` do not shrink map storage; copy survivors into a new map to release it. Large inline values are
copied on reads; pointer values trade those copies for allocations and GC scanning. Measure at realistic cardinality.

## Struct layout and locality

Reorder fields only for measured hot paths or high object counts; otherwise group by meaning. A trailing zero-size
field adds padding. Pad counters for false sharing only when measurements show scaling gets worse across cores.
Prefer slices to linked lists unless removal through a held handle is required; profile before assembly or SIMD,
keep a pure-Go fallback, and batch cgo calls.

## Runtime tuning

`GOMEMLIMIT` is soft and excludes cgo/mmap memory; leave container headroom. It cannot save a live set larger than
the limit. Go 1.25+ adapts `GOMAXPROCS` to cgroup limits; remeasure GC tuning after runtime upgrades. Memory ballast
is obsolete. PGO needs representative CPU profiles and refreshing after large refactors, not a synthetic hot loop.
