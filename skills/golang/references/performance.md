# Go Performance

Finding and fixing Go hot spots on Go 1.27: profiling, allocation, data layout, collection internals, runtime knobs.

## Measure first

- Rule out external waits first: many goroutines blocked in `net.(*conn).Read` or `database/sql`, or a trace span
  dominated by an upstream, means fix the query, pool or cache.
- Pick one metric (p99, throughput, RSS, CPU) and a target; one change per measurement; compare with `benchstat`
  over `-count` ≥ 6. Benchmarks: [testing.md](testing.md).
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
- Compile `regexp` once at package level.
- Concurrent misses on one key: `singleflight` ([concurrency.md](concurrency.md)).

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

- Growth doubles while small, then ~1.25× (switch near 256 elements), rounded to a malloc size class; exact sizes
  change between releases, so never depend on when reallocation happens.
- `append` into a subslice with spare capacity overwrites the parent's later elements: hand out `s[lo:hi:hi]` or
  `slices.Clone` when the receiver may append.
- `slices.Delete`, `Compact`, `DeleteFunc` (1.22+) zero the vacated tail; hand-written `s = s[:n]` keeps removed
  pointers reachable.
- `slices.Clip` drops spare capacity but not the array; only a copy shrinks it.
- An array (`[32]byte`) is comparable and a valid map key; pass large ones by pointer.
- Matrices: one allocation (`make([]float64, rows*cols)`, rows as subslices), walked row-major.

## Maps: internals and gotchas

- Go 1.24+ maps are Swiss tables: 8-slot groups with a control word of hash fingerprints (one probe checks 8
  slots), in tables of ≤1024 slots that grow independently, so no long rehash pause.
- Maps never shrink: `delete` and `clear(m)` keep the tables. To free memory, copy into `make(map[K]V, len(old))`.
- Reads copy the value: for large, frequently changing values use `map[K]*V` (one heap object per entry, more to
  scan); small or mostly-read values stay inline.
- `slices.Contains` in a loop is O(n·m): build a `map[T]struct{}` once.
- Pointer-free keys and values (ints, fixed arrays) spare the GC from scanning the map; matters at millions of entries.
- `sync.Map` only for write-once/read-many or disjoint keys: [concurrency.md](concurrency.md).

## Struct layout and locality

- Fields keep declared order and pad to alignment; order largest to smallest. `fieldalignment`
  (`golang.org/x/tools`) reports shrinkable structs.

```go
type loose struct { a bool; b int64; c bool; d int32 } // 24 bytes: 7 + 3 bytes of padding
type tight struct { b int64; d int32; a, c bool }      // 16 bytes
```

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
- A cgo call costs tens of ns and pins the goroutine to its OS thread: batch across the boundary.
- A disabled debug log line still formats its arguments: `slog.LogAttrs` with typed attributes (`slog.Int`,
  `slog.String`) avoids allocating when the level is off.

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
- Go 1.26+ uses the Green Tea collector (page-by-page small-object scanning, better locality); re-measure GC CPU
  instead of reusing old tuning.
- A finished goroutine keeps its large stack until GC shrinks it: deep recursion in pooled workers shows as high
  `StackInuse`.
- PGO: save a 30–60 s CPU profile of representative production load as `default.pgo` in the main package directory;
  `go build` picks it up (`-pgo=auto`). Expect a few percent, mostly devirtualized interface calls. Merge with
  `go tool pprof -proto a.pprof b.pprof > default.pgo`; refresh after large refactors (stale helps less, no harm).
- `weak.Pointer[T]` and `runtime.AddCleanup` (1.24+) build GC-reclaimable caches and replace `SetFinalizer`; cleanups
  never resurrect the object, and one object can have several.
