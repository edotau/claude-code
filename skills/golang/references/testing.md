# Go Testing

Stdlib test mechanics, fuzzing, golden files, HTTP, races and benchmarks.

## Layout

- `package foo_test` tests only the public API and breaks import cycles; examples read best external.
- Gate slow or environment-bound tests at runtime (`testing.Short()`, missing binary, `runtime.GOOS`) with `t.Skip`
  and a reason; a `//go:build integration` tag hides the file from `go vet` and gopls unless set.
- For a real boundary (clock, network, exec), a package-level func var swapped in the test and restored in
  `t.Cleanup` is often lighter than an interface added for a fake. The swap is process-global: no `t.Parallel()`.

## Naming and selection

| Form | Name | Run one |
| --- | --- | --- |
| Test | `TestParse`, `TestStore_Get` | `go test -run '^TestStore_Get$' ./internal/store` |
| Subtest | `t.Run("empty input", …)` → `TestParse/empty_input` | `-run 'TestParse/empty_input'` |
| Fuzz | `FuzzParse` | `-run '^$' -fuzz '^FuzzParse$' -fuzztime 30s` |
| Benchmark | `BenchmarkParse/size=4k` | `-run '^$' -bench 'Parse/size=4k' -benchmem` |
| Example | `ExampleStore_Get_cached` (suffix lowercase) | `-run ExampleStore` |

- `-run` splits on `/` and matches each level; `-run '^$'` skips tests so only benchmarks or fuzzing run.
- `-count=1` defeats the test cache; `-shuffle=on` exposes order dependence; `-failfast` stops at first failure.

## Table tests and subtests

- The case `name` says the condition, not the expected value (`"trailing slash"`, not `"ok"`).

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

## Parallelism and the race detector

- `t.Parallel()` only for tests touching no package var, env, cwd or shared fixture file; it pauses the test until
  serial siblings finish, so a parent's `defer` runs before parallel children: use `t.Cleanup`.
- Run touched packages under `go test -race` when the change involves goroutines, channels or shared state; it only
  sees races the run executes, so drive the concurrent path (N goroutines, a barrier) on purpose.

## Time and goroutines: `testing/synctest`

- `synctest.Test(t, func(t *testing.T) { … })` runs the body in a bubble with a fake clock that advances only when
  every bubbled goroutine is durably blocked: timers, `time.Sleep`, `context.WithTimeout` become instant and exact.
- `synctest.Wait()` blocks until all other bubbled goroutines are blocked: the sync point before asserting.
- Blocking on I/O, a mutex or a channel created outside the bubble is not "durable" and stalls the clock. Use
  `net.Pipe` or `httptest.NewTestServer` (in-memory network).

```go
func TestRetryBacksOff(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        start := time.Now()
        _ = retry(t.Context(), 4, func() error { return errBusy })
        if got := time.Since(start); got != 7*time.Second { // 4 attempts, backoffs 1s + 2s + 4s
            t.Errorf("elapsed %v, want 7s", got)
        }
    })
}
```

## HTTP

| Need | Tool |
| --- | --- |
| One handler, no network | `httptest.NewRequest` + `httptest.NewRecorder`, then `h.ServeHTTP(rec, req)` |
| A real client against a fake upstream | `httptest.NewTestServer(t, h)`: in-memory, auto-closed, synctest-safe |
| Real loopback socket (TLS, proxy) | `httptest.NewTLSServer` + `t.Cleanup(srv.Close)`; `srv.Client()` trusts it |

## Golden files

- Fixtures in `testdata/` (the toolchain ignores it); goldens are `testdata/<name>.golden`.
- One package-level `var update = flag.Bool("update", false, "rewrite golden files")` in the test file of the source
  that owns the output. Regenerate with `go test ./pkg -run TestName -update`, then review the diff.
- On mismatch print a diff and the regenerate command; never auto-update on failure.
- Normalize before compare: sort map-derived output, replace temp paths and timestamps with fixed tokens, `\r\n` to
  `\n`.

## Fuzzing

- Seed with `f.Add` from real inputs and regression cases; arguments limited to `[]byte`, `string`, bools, ints,
  uints, floats and runes.
- Assert properties: round-trip (`decode(encode(x)) == x`), no panic, invariants, agreement with a slow reference.
- A crash writes `testdata/fuzz/FuzzX/<hash>`; commit it: plain `go test` replays the corpus.
- `-fuzz` takes exactly one target per package; bound runs with `-fuzztime 30s` (or `100000x`) outside CI.

## Examples

- `ExampleFoo` with `// Output:` is a test (exact stdout, outer whitespace trimmed); `// Unordered output:` for map
  or concurrent output. No output comment means compiled but never run, so not a guard.

## Coverage

- `-covermode=atomic` with `-race`; `-coverpkg=./...` credits packages exercised from another package's tests.

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

## Comparing with benchstat

```bash
cd /path/to/repo
go test -run '^$' -bench 'Parse' -benchmem -count=10 ./internal/parse > old.txt
# apply the change, same machine, same toolchain, nothing else running
go test -run '^$' -bench 'Parse' -benchmem -count=10 ./internal/parse > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

- Read the median, `± N%` (95% CI; above ~5% the machine is noisy) and `p=`; `~` means no significant change: do
  not claim one. Raise `-count` or cut noise first.
- Recording the delta in the commit: [performance.md](performance.md).

## testify (optional)

Stdlib unless the target repo already imports testify. When it does:

- `require` for preconditions (`FailNow`), `assert` for the rest; arguments are `(expected, actual)`.
- Build `assert.New(t)` inside each `t.Run` from the subtest's `t`; one bound to the parent attributes failures to
  the parent while the subtest reports PASS.
- `mock.Mock` only at real boundaries, always `AssertExpectations(t)`; a seam var holding a closure is usually
  shorter. Skip `suite`: `t.Run` plus `t.Cleanup` covers it.
