# Go Performance

Finding and fixing Go hot spots on Go 1.27: profiling, allocation, data layout, collection internals, runtime knobs.

## Measure first

- Rule out waits outside the process first. Lots of goroutines blocked in `net.(*conn).Read` or `database/sql`, or a
  trace span dominated by an upstream, means the fix is the query, pool or cache, not your allocations.
- Pick one metric (p99 latency, throughput, RSS, CPU) and a target before changing code; make one change per
  measurement and compare with `benchstat` across `-count` ≥ 6 runs. Running and reading benchmarks:
  [testing.md](testing.md).
- Put the `benchstat` delta (trimmed to the changed rows, with the `goos/goarch/cpu` header) in the `perf(scope):`
  commit body and a one-line comment on the optimized code, so nobody "simplifies" it back later.

| Question | Tool | Look at |
| --- | --- | --- |
| Where does CPU go? | `go test -cpuprofile` · `/debug/pprof/profile?seconds=30` | `top -cum`, flame graph |
| Who allocates most? | `pprof -sample_index=alloc_objects` (`alloc_space` for bytes) | hot handlers, encoders |
| What is holding live memory? | `pprof -sample_index=inuse_space` | large live objects that should be gone |
| Why is it waiting? | block, mutex and goroutine profiles · `fgprof` (on- and off-CPU) | lock and channel waits, I/O |
| Scheduling, GC, latency | `go tool trace` · `trace.FlightRecorder` (1.25+, last N seconds) | STW, assist, idle P |
| Does it escape? Is it inlined? | `go build -gcflags='-m=2' ./pkg 2>&1` | `escapes to heap`, `cannot inline` |
| How often does GC run? | `GODEBUG=gctrace=1` · `runtime/metrics` | GC CPU %, heap goal, cycles/s |

- Compare two profiles with `pprof -diff_base=old.pprof new.pprof`; tag requests with `pprof.Do(ctx, labels, fn)` to
  split one handler's samples from another's.
- If profiles show `runtime.mallocgc` and `gcBgMarkWorker` at the top, cut allocations first; tune the runtime
  (below) only after that.

## Allocations and escape analysis

- A value escapes when its address is returned, stored in a heap object or interface, captured by an escaping
  closure (such as a `go` statement), or its size is unknown at compile time or too large for the stack.
- Read `-m=2` for the reason: `leaking param: p` means the caller's argument now escapes too. The fix belongs at
  the API: accept a destination (`func Fill(dst *T)`, `AppendX(dst []byte, …) []byte`) instead of returning a fresh
  pointer.
- Converting a non-pointer value to `any` can allocate (the runtime avoids it for small integers and
  zero-size values). `fmt.*`, `slog.Any` and `[]any` parameters in a hot loop allocate per call; use typed code or
  generics instead.
- Use `strconv.AppendInt`/`AppendQuote`, `time.AppendFormat` and `fmt.Appendf` into a reused buffer instead of the
  versions that return a new string.
- In hot validation paths, return sentinel errors (`var ErrX = errors.New(…)`); `fmt.Errorf` allocates on every call.
  Add dynamic context only where the value carries information.
- Go 1.25+ can put a small variable-sized `make([]T, n)` on the stack. Check with `-m` rather than assuming the
  allocation is gone.
- Reflection (`reflect.Value`, `reflect.DeepEqual`) costs one to two orders of magnitude more than typed code: use
  `slices.Equal`, `maps.Equal` and `bytes.Equal`, and use a type switch instead of a chain of assertions.

## Preallocate and reuse

- Output the same length as the input: `make([]T, len(in))` with index assignment beats append (no length update per
  element). Keep append for filtering, or when an early return should leave nothing half-filled.
- `make(map[K]V, n)` when the final count is known: it sizes the table once instead of growing it during inserts.
- Range over the key and value together (`for k, v := range m`); `for k := range m { m[k] }` hashes the key twice.
- Recompiling a `regexp` on every call costs more than the match: compile once at package level.
- Concurrent misses on one key: `singleflight` ([concurrency.md](concurrency.md)).

## Strings and bytes

| Need | Use | Why |
| --- | --- | --- |
| Build a string in a loop | `strings.Builder` + `Grow(n)` | `String()` hands over the bytes without copying |
| Read and write a buffer, `io` plumbing | `bytes.Buffer` | both `io.Reader` and `io.Writer`; `String()` copies |
| Join a few fixed parts | `a + b + c` | one allocation for the whole expression |
| `+=` in a loop | never | O(n²) copying |

- Copying a `strings.Builder` after its first write panics: pass `*strings.Builder`.
- The compiler doesn't allocate for `m[string(b)]` lookups, `string(b) == s` comparisons, or ranging over
  `[]byte(s)`. Everywhere else, `string`↔`[]byte` copies — convert once and keep the result.
- A substring or subslice keeps the whole parent alive. To keep 8 bytes of a 1 MiB payload, use `strings.Clone` or
  `bytes.Clone`.
- Use `unsafe.String`/`unsafe.StringData` only when the bytes are never modified afterwards; otherwise a string that
  should be immutable changes under its readers.
- Interning: `unique.Make` (1.23+) deduplicates repeated strings, and comparing the returned handles is a single
  pointer compare.

## sync.Pool

- Use it for short-lived, same-shape objects allocated at a high rate (encode buffers, per-request scratch). For rare
  or long-lived objects, the pool costs more than the allocation it saves.
- Pooled objects are dropped over two GC cycles (there is a victim cache), so a pool cannot serve as a cache or a
  limit on live objects. Never assume `Get` hands back a zeroed or surviving object (`New` runs only on an empty
  pool).
- Store pointers: `Put(&buf)`, not `Put(buf)`. Converting a slice header to `any` allocates (staticcheck SA6002).
- Reset before `Put`: truncate the slice and clear references to other objects; anything still referenced stays
  alive.
- Discard objects that grew past a cap instead of putting them back (`if cap(*bp) > 64<<10 { return }`), so one
  huge request doesn't keep its memory in the pool.
- Never return pooled memory to the caller; copy the result out first. Otherwise a later `Get` overwrites data the
  caller still holds.

```go
var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4<<10); return &b }}

func render(v Item) []byte {
    bp := bufPool.Get().(*[]byte)
    buf := v.AppendTo((*bp)[:0])
    out := bytes.Clone(buf)  // the caller gets its own copy
    if cap(buf) <= 64<<10 { // drop oversized buffers
        *bp = buf
        bufPool.Put(bp)
    }
    return out
}
```

## Slices: internals and gotchas

- Growth roughly doubles while small, then grows about 1.25× (the switch is near 256 elements) and rounds up to a
  malloc size class. The exact sizes change between releases — never write code that depends on when a reallocation
  happens.
- `append` into a subslice that still has spare capacity overwrites the parent's later elements. Hand out
  `s[lo:hi:hi]` (a full slice expression) or `slices.Clone` when the receiver may append.
- `slices.Delete`, `Compact` and `DeleteFunc` (1.22+) zero the tail they vacate, so removed pointers can be
  collected. A hand-written `s = s[:n]` keeps them reachable.
- `slices.Clip` drops unused capacity, but the array stays the same size. Only a copy shrinks it.
- An array (`[32]byte`) for compile-time sizes is comparable and usable as a map key; pass large ones by pointer.
- Lay out matrices in one allocation (`data := make([]float64, rows*cols)`, rows as subslices) and walk them in
  row-major order. Column-first walks on big matrices miss the cache on every step.

## Maps: internals and gotchas

- Since Go 1.24, maps are Swiss tables: 8-slot groups, each with a control word of hash fingerprints so one probe
  checks 8 slots, split into tables of at most 1024 slots that grow independently. That keeps growth incremental,
  with no single long rehash pause.
- Maps never shrink. A map that once held 10M entries keeps its tables after `delete`, and `clear(m)` empties it
  without freeing memory. To give the memory back, copy into a new map with `make(map[K]V, len(old))`.
- Every map read copies the value. For large values that change often, use `map[K]*V`, accepting one heap object per
  entry and more pointers to scan. Small or mostly-read values stay inline.
- `slices.Contains` inside a loop is O(n·m): build a `map[T]struct{}` set once.
- Pointer-free keys and values (ints, fixed arrays, structs without pointers) save the GC from scanning the map at
  all. That matters for maps with millions of entries.
- `sync.Map` pays off only for write-once/read-many or disjoint-key workloads. Choosing it:
  [concurrency.md](concurrency.md).

## Struct layout and locality

- The compiler keeps field order and pads each field to its alignment (1, 2, 4, 8 bytes on 64-bit). Order fields
  from largest to smallest; `fieldalignment` (in `golang.org/x/tools`) reports structs that would shrink.

```go
type loose struct { a bool; b int64; c bool; d int32 } // 24 bytes: 7 + 3 bytes of padding
type tight struct { b int64; d int32; a, c bool }      // 16 bytes
```

- A zero-size field placed last gets padded (`struct{ v int64; _ struct{} }` is 16 bytes), so that a pointer to it
  can't point past the object. Put it first.
- Reorder only structs that are allocated by the million or iterated in hot loops. Elsewhere, grouping fields by
  meaning is worth more than the saved bytes.
- When a loop reads only one or two fields, a struct of arrays (`xs, ys []float64`) beats an array of structs:
  every byte loaded into the cache gets used.
- Store nodes in a `[]Node` and link them with `int32` indices instead of pointers. The nodes stay contiguous and the
  GC has nothing to scan.
- False sharing: counters that different cores write and that share a 64-byte line slow each other down. Pad them
  apart (`_ [56]byte`) only when a benchmark shows adding goroutines makes it slower.
- `container/list` loses to a slice for nearly every workload (one allocation and cache miss per node); keep it only
  for O(1) removal through a handle you already hold (the LRU pattern). Priority queues: `container/heap`.

## CPU micro-patterns

- Inlining: check `-m` for `cannot inline … function too complex` on hot leaf functions. Move logging and metrics
  calls out of small helpers, because they push the function over the inlining budget.
- Bounds checks: index the last element first (`_ = s[3]`) or loop over `range s` so the compiler can drop the
  checks. Confirm with `-gcflags='-d=ssa/check_bce'`.
- Loops that sum into one accumulator form a dependency chain. Use 2–4 independent accumulators when a profile shows
  the loop itself is the cost.
- `math/bits` functions compile to single instructions (popcount, clz, rotate). Hand-written assembly or the
  `GOEXPERIMENT=simd` packages are justified only by a profiled numeric kernel, with a pure-Go fallback.
- Each cgo call costs tens of nanoseconds and pins the goroutine to its OS thread for the duration. Pass whole
  batches across the boundary, never one element per call.
- Logging: a disabled debug line still formats its arguments. `slog.LogAttrs` with typed attributes
  (`slog.Int`, `slog.String`) avoids allocating when the level is off.

## I/O

- Check the error from `bufio.Writer.Flush`; raise `bufio.Scanner`'s 64 KiB token limit with `Buffer`; stream with
  `io.Copy` or `json.Decoder` instead of `io.ReadAll` on unbounded input.
- `http.Transport` keeps only 2 idle connections per host by default (`MaxIdleConnsPerHost`). Raise it to match your
  concurrency, share one client, and read the body to EOF before `Close` so the connection goes back to the pool.
- Batch round-trips (multi-row insert, `COPY`, bulk API calls). Per-call overhead usually costs more than anything
  you can shave off the payload.

## Runtime tuning

| Knob | Effect | Use when |
| --- | --- | --- |
| `GOGC` (default 100) | next GC when the heap grows by GOGC% over the live heap | trading memory for fewer cycles |
| `GOMEMLIMIT` | soft limit on total Go memory; GC runs harder as usage nears it | container limits, avoiding OOM |
| `GOGC=off` + `GOMEMLIMIT` | GC runs only near the limit | batch jobs with a known, stable working set |
| `GOMAXPROCS` | number of Ps; follows the cgroup CPU limit and its changes (1.25+) | rarely set by hand now |
| `default.pgo` | profile-guided inlining and devirtualization | long-running services with a stable profile |

- Set `GOMEMLIMIT` to about 80–90% of the container limit. Stacks, cgo allocations and mmap count toward the
  container but sit outside the Go heap. The limit is soft: the GC caps itself at about 50% CPU, so a live heap
  above the limit still runs out of memory instead of looping.
- At runtime, set both knobs with `debug.SetGCPercent` and `debug.SetMemoryLimit`. Memory ballast is obsolete;
  delete it.
- Go 1.26+ uses the Green Tea collector by default, which scans small objects page by page for better locality.
  Re-measure GC CPU after upgrading rather than reusing old tuning.
- A finished goroutine keeps its large stack until the GC shrinks it. Deep recursion inside a pool of worker
  goroutines shows up as high `StackInuse`.
- PGO: save a 30–60 s CPU profile of representative production load as `default.pgo` in the main package
  directory. `go build` picks it up (`-pgo=auto`). Expect a few percent, mostly from devirtualized interface calls.
  Merge several profiles with `go tool pprof -proto a.pprof b.pprof > default.pgo`, and refresh after large
  refactors; a stale profile helps less but does no harm.
- `weak.Pointer[T]` and `runtime.AddCleanup` (1.24+) build caches the GC can reclaim, and replace `SetFinalizer`.
  Cleanups never resurrect the object, and one object can have several.
