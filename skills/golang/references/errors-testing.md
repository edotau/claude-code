# Go Errors and Testing

Creating, wrapping, inspecting, logging and surfacing errors; stdlib test mechanics, fuzzing, goldens, HTTP, races and
benchmarks (Go 1.22+).

## Core rules

- Never discard an error with `_` without a comment saying why it can't matter.
- Handle an error once: log it **or** return it, never both.
- `io.Reader.Read` may return `n > 0` with an error; process the bytes first.
- Never return a typed nil pointer as `error`: the interface is non-nil (`var e *NotFoundError; return e` is wrong;
  return `nil` explicitly).
- Shadowing: `if x, err := f(); err != nil` leaves the outer `err` untouched (a named return may still be nil).
- `err` reused across calls with no check between silently loses the first failure.
- Never mutate or reassign an `Err*` var.
- `errors.Is(nil, ErrX)` is false and `errors.Join()` is nil: no special-casing.

## Message style

- Describe the operation, not the failure: `"load config: %w"`, not `"failed to load config: %w"`.
- No package or type prefix (`"UserService: ..."`); the wrap chain says where.
- State what happened, not what to do; remediation belongs to the layer that talks to the user.
- Add context the caller lacks (path, ID, key); don't repeat arguments it passed.
- Keep messages low-cardinality: variable data goes in structured attributes or error fields (a static wrap prefix
  already is). Data known only deep in the stack: fields on a custom type, not only interpolated text.
- Never put secrets, tokens or PII in an error string.

## Creating errors

| Need | Use | Caller checks with |
| --- | --- | --- |
| Fixed condition callers branch on | sentinel: `var ErrNotFound = errors.New("not found")` | `errors.Is` |
| Condition with data callers read | custom type: `type ValidationError struct{ Field string }` | `errors.AsType` |
| One-off, nobody branches | `errors.New` / `fmt.Errorf` inline | nothing |
| Add context to an underlying error | `fmt.Errorf("op %s: %w", id, err)` | `Is`/`As` see through it |
| Several independent failures | `errors.Join(errs...)` | `Is`/`As` match any member |

- Sentinels and custom types become public API: export only what callers branch on.
- Pointer receiver for `Error()`, returned as a pointer, so `As` targets are consistent.
- `Unwrap() error` on a custom type carrying a cause, or `Is`/`As` stop at it.
- `Is(target error) bool` only when equality is semantic (match by code), not to fake a hierarchy.
- Across packages prefer behaviour interfaces (`interface{ Temporary() bool }` via `errors.As`) to concrete types.
- Hot paths: sentinels over `fmt.Errorf` ([performance.md](performance.md)).
- stdlib errors carry no stack. For an unexpected fault capture `runtime.Callers` in a custom type at the origin, not
  per wrap.

- A custom type can also match its sentinel: `func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound }`.

## Wrapping

- `%w` inside your module; at a package/API boundary use `%v` (or translate) when exposing the cause would freeze an
  implementation detail (a `*pq.Error` leaking through a repository).
- `%v` still copies the cause's text. To hide internals, translate to your own sentinel or type
  (`fmt.Errorf("get user %s: %w", id, ErrUnavailable)`) and log the original.
- Multiple `%w` (1.20+) expose all via `Unwrap() []error`: `fmt.Errorf("sync %s: %w: %w", id, ErrConflict, err)`.
- Wrap once per layer that adds information, at least per package boundary; otherwise return `err` unchanged.
- `fmt.Errorf("x: %w", nil)` returns a non-nil error with text `x: %!w(<nil>)`.

## Inspecting

- Never `==`, `err.(*T)` or match `err.Error()` text: all break once anything wraps. `errors.Is` walks the whole
  chain, including `Join` and multi-`%w` trees.
- Go 1.26+: `errors.AsType[T](err) (T, bool)` replaces the pointer-target dance and can't panic on a bad target.

```go
if nf, ok := errors.AsType[*NotFoundError](err); ok {
    return notFound(nf.Kind, nf.ID)
}
// pre-1.26: var nf *NotFoundError; if errors.As(err, &nf) { ... }  — target must be a non-nil pointer
```

- `AsType`'s `T` must implement `error`; a behaviour interface still needs `errors.As` with a pointer to an
  interface variable.
- `errors.Unwrap` returns nil for multi-errors; use `Is`/`As`.
- Check `context.Canceled` / `DeadlineExceeded` with `errors.Is` and usually stop quietly. `context.Cause(ctx)` gives
  the `WithCancelCause` reason.
- Retry only classified transient errors (`errors.Is` / behaviour interface); retrying validation or auth errors just
  delays the failure.

## Aggregating

- `errors.Join` drops nil arguments and returns nil when all are nil: safe to accumulate unconditionally. Text is
  members joined by newlines; prefix-wrap it for single-line logs.
- Concurrent code: per-worker error slice (no shared mutable error), then join; `errgroup` when the first error
  should cancel the rest.

## Deferred cleanup

- `defer f.Close()` on a file you **wrote** loses the write error (buffered data flushes at close). Capture it, and
  never let cleanup overwrite an earlier error with nil; `Join` keeps both. Read-only handles: plain `defer Close()`.

```go
func write(path string, b []byte) (err error) {
    f, err := os.Create(path)
    if err != nil {
        return fmt.Errorf("create %s: %w", path, err)
    }
    defer func() { err = errors.Join(err, f.Close()) }()
    _, err = f.Write(b)
    return err
}
```

## Panics

| Situation | Do |
| --- | --- |
| Expected failure (I/O, input, network, not found) | return an `error` |
| Programmer bug / broken invariant (impossible switch arm, nil required dep) | `panic` with a clear message |
| `Must*` helper for init-time constants (`regexp.MustCompile`) | panic is fine: input is a literal |
| Goroutine you spawn | `recover` at its top if the process must survive; an unrecovered panic kills it |
| Library API | never let a panic cross the public boundary; convert to `error` |

- `recover` only works in a function deferred directly by the panicking goroutine.
- Recovered `r` that is an `error`: wrap with `%w`; capture `debug.Stack()` inside the deferred function (gone after
  it returns).
- Don't recover-and-continue on `runtime.Error` in core state code; the invariant is broken.
- HTTP recovery middleware: log value and stack, answer 500 with a generic body, re-panic `http.ErrAbortHandler`
  (net/http's deliberate abort).

## Logging and surfacing

- Log at the top of the call stack (handler, `main`, worker loop) where the error stops propagating; lower layers
  wrap and return.
- `slog.Error("sync failed", "err", err, "repo", id)`: constant message, never `err.Error()` (a dynamic message makes
  every failure its own group); variable data in attributes.
- A field-carrying error type gets `LogValue() slog.Value` (`slog.LogValuer`); request-scoped data (trace ID, user)
  comes from `ctx` at the log site, not every wrap.
- Logging an attempt you then retry is handling it; return only the final failure, wrapped with the attempt count.
- Level by who must act: client errors (bad input, not found) Info/Warn or none; server faults Error.
- HTTP/RPC boundary: map domain errors to status codes in one place (`errors.Is` switch); send a safe message, never
  the raw chain. A user-facing message lives in a field, extracted with `AsType`, generic fallback otherwise.
- CLI: one line to stderr, non-zero exit from `main`; code mapping: [tools.md](tools.md).

## Test layout

- `package foo_test` tests only the public API and breaks import cycles; examples read best external.
- Gate slow or environment-bound tests at runtime (`testing.Short()`, missing binary, `runtime.GOOS`) with `t.Skip`
  and a reason; a `//go:build integration` tag hides the file from `go vet` and gopls unless set.
- For a real boundary (clock, network, exec), a package-level func var swapped in the test and restored in
  `t.Cleanup` is often lighter than an interface added for a fake. The swap is process-global: no `t.Parallel()`.
- Test error paths first; assert with `errors.Is` / `errors.AsType`, never message text.
- Table cases: `name` says the condition, not the expected value (`"trailing slash"`, not `"ok"`).

| Form | Name | Run one |
| --- | --- | --- |
| Test | `TestParse`, `TestStore_Get` | `rtk go test -run '^TestStore_Get$' ./internal/store` |
| Subtest | `t.Run("empty input", …)` → `TestParse/empty_input` | `-run 'TestParse/empty_input'` |
| Fuzz | `FuzzParse` | `-run '^$' -fuzz '^FuzzParse$' -fuzztime 30s` |
| Benchmark | `BenchmarkParse/size=4k` | `-run '^$' -bench 'Parse/size=4k' -benchmem` |
| Example | `ExampleStore_Get_cached` (suffix lowercase) | `-run ExampleStore` |

- `-run` splits on `/` and matches each level; `-run '^$'` skips tests so only benchmarks or fuzzing run.
- `-count=1` defeats the test cache; `-shuffle=on` exposes order dependence; `-failfast` stops at first failure.

## Lifecycle helpers on `testing.T`

| Helper | Use for | Gotcha |
| --- | --- | --- |
| `t.Helper()` | First line of any assert/setup helper | Omit it and failures blame the helper's line |
| `t.Cleanup(fn)` | Teardown next to setup; LIFO; runs after subtests | `defer` fires before parallel subtests run |
| `t.Context()` | Context canceled just before Cleanup runs | Goroutines outliving the test should watch it |
| `t.TempDir()` | Hermetic dir, removed after the test | Point `HOME` and config-dir env vars at it, never the real one |
| `t.Setenv`, `t.Chdir` | Process-wide env/cwd, restored after | Panics under `t.Parallel()` |
| `t.ArtifactDir()` | Files kept for inspection (`-artifacts` flag) | Not a scratch dir |
| `t.Output()` | `io.Writer` into the test log, indented like `t.Log` | Give it to a logger instead of `os.Stderr` |

## Parallelism, races and time

- `t.Parallel()` only for tests touching no package var, env, cwd or shared fixture file; it pauses the test until
  serial siblings finish, so a parent's `defer` runs before parallel children: use `t.Cleanup`.
- Run touched packages under `go test -race` when the change involves goroutines, channels or shared state; it only
  sees races the run executes, so drive the concurrent path (N goroutines, a barrier) on purpose.
- `synctest.Test(t, func(t *testing.T) { … })` runs the body in a bubble with a fake clock that advances only when
  every bubbled goroutine is durably blocked: timers, `time.Sleep`, `context.WithTimeout` become instant and exact.
- `synctest.Wait()` blocks until all other bubbled goroutines are blocked: the sync point before asserting.
- Blocking on I/O, a mutex or a channel created outside the bubble is not "durable" and stalls the clock. Use
  `net.Pipe` or `httptest.NewTestServer` (in-memory network).

- Example: wrap `retry(t.Context(), 4, fail)` in `synctest.Test`; assert `time.Since(start) == 7*time.Second`
  (backoffs 1s + 2s + 4s) with no real wait.

## HTTP

| Need | Tool |
| --- | --- |
| One handler, no network | `httptest.NewRequest` + `httptest.NewRecorder`, then `h.ServeHTTP(rec, req)` |
| A real client against a fake upstream | `httptest.NewTestServer(t, h)`: in-memory, auto-closed, synctest-safe |
| Real loopback socket (TLS, proxy) | `httptest.NewTLSServer` + `t.Cleanup(srv.Close)`; `srv.Client()` trusts it |

## Golden files, fuzzing, examples, coverage

- Fixtures in `testdata/` (the toolchain ignores it); goldens are `testdata/<name>.golden`.
- One package-level `var update = flag.Bool("update", false, "rewrite golden files")` in the test file of the source
  that owns the output. Regenerate with `rtk go test ./pkg -run TestName -update`, then review the diff.
- On mismatch print a diff and the regenerate command; never auto-update on failure.
- Normalize before compare: sort map-derived output, replace temp paths and timestamps with fixed tokens, `\r\n` to
  `\n`.
- Fuzz: seed with `f.Add` from real inputs and regression cases; arguments limited to `[]byte`, `string`, bools,
  ints, uints, floats and runes.
- Fuzz properties: round-trip (`decode(encode(x)) == x`), no panic, invariants, agreement with a slow reference.
- A fuzz crash writes `testdata/fuzz/FuzzX/<hash>`; commit it: plain `go test` replays the corpus.
- `-fuzz` takes exactly one target per package; bound runs with `-fuzztime 30s` (or `100000x`) outside CI.
- `ExampleFoo` with `// Output:` is a test (exact stdout, outer whitespace trimmed); `// Unordered output:` for map
  or concurrent output. No output comment means compiled but never run, so not a guard.
- Coverage: `-covermode=atomic` with `-race`; `-coverpkg=./...` credits packages exercised from another package's
  tests.

## Benchmarks

- New ones use `for b.Loop() { … }`: setup above the loop is excluded and the body's calls stay alive, so no
  `b.ResetTimer` or result sink. Legacy `for range b.N` may stay.
- `b.ReportAllocs()` (or `-benchmem`) for B/op and allocs/op; `b.ReportMetric(v, "unit")` for domain metrics.
- One `b.Run` per variant or size (`size=64`, `size=4k`): the sub-name is the row benchstat compares, and a sweep
  exposes superlinear growth.

| Flag | Effect |
| --- | --- |
| `-count=10` | Samples for statistics; under 6 benchstat cannot reach significance |
| `-benchtime=2s` / `500x` | Time or fixed iterations per sample |
| `-cpu=1,4,8` | Repeat at several GOMAXPROCS (`-8` suffix in the name) |
| `-cpuprofile`, `-memprofile`, `-trace` | Profile the benchmark; read with `go tool pprof` / `go tool trace` |

- Workflow: `go test -run '^$' -bench Parse -benchmem -count=10 ./pkg > old.txt`, apply the change (same machine,
  toolchain, nothing else running), repeat to `new.txt`, then `go run golang.org/x/perf/cmd/benchstat@latest old.txt
  new.txt`.
- Read the median, `± N%` (95% CI; above ~5% the machine is noisy) and `p=`; `~` means no significant change: do
  not claim one. Raise `-count` or cut noise first. Recording the delta in the commit: [performance.md](performance.md).

## testify (optional)

Stdlib unless the target repo already imports testify. When it does:

- `require` for preconditions (`FailNow`), `assert` for the rest; arguments are `(expected, actual)`.
- Build `assert.New(t)` inside each `t.Run` from the subtest's `t`; one bound to the parent attributes failures to
  the parent while the subtest reports PASS.
- `mock.Mock` only at real boundaries, always `AssertExpectations(t)`; a seam var holding a closure is usually
  shorter. Skip `suite`: `t.Run` plus `t.Cleanup` covers it.
