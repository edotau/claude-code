# Go Testing

Stdlib test mechanics, fuzzing, golden files, HTTP, races and benchmarks in Go.

## Layout

- `package foo_test` (external) tests only the public API and breaks import cycles when a test needs a package that
  imports `foo`. Examples read best external, since they show the call as a user writes it.
- Prefer gating slow or environment-bound tests at runtime (`testing.Short()`, a missing binary, `runtime.GOOS`) with
  `t.Skip` and a reason; a `//go:build integration` tag hides the file from `go vet` and gopls unless the tag is set.
- For a real boundary (clock, network, exec), a package-level func var swapped in the test and restored in
  `t.Cleanup` is often lighter than an interface added only for a fake. The swap is process-global: those tests must
  not call `t.Parallel()`.

## Naming and selection

| Form | Name | Run one |
| --- | --- | --- |
| Test | `TestParse`, `TestStore_Get` | `go test -run '^TestStore_Get$' ./internal/store` |
| Subtest | `t.Run("empty input", …)` → `TestParse/empty_input` | `-run 'TestParse/empty_input'` |
| Fuzz | `FuzzParse` | `-run '^$' -fuzz '^FuzzParse$' -fuzztime 30s` |
| Benchmark | `BenchmarkParse/size=4k` | `-run '^$' -bench 'Parse/size=4k' -benchmem` |
| Example | `ExampleStore_Get_cached` (suffix lowercase) | `-run ExampleStore` |

- `-run` splits on `/` and matches each level separately; `-run '^$'` skips tests so only benchmarks or fuzzing run.
- `-count=1` defeats the test cache; `-shuffle=on` exposes order dependence; `-failfast` stops at the first failure.

## Table tests and subtests

- The case `name` says the condition, not the expected value (`"trailing slash"`, not `"ok"`).
- Keep `want` and `wantErr` in the row; assert the error with `errors.Is`/`errors.As`, never a string compare unless
  the message itself is the contract.
- Split a table whose rows need branching setup (`if tt.mockFails …`) into separate tests: one table, one shape.

## Lifecycle helpers on `testing.T`

| Helper | Use for | Gotcha |
| --- | --- | --- |
| `t.Helper()` | First line of any assert/setup helper | Omit it and every failure blames the helper's line |
| `t.Cleanup(fn)` | Teardown next to setup; LIFO; runs after subtests | `defer` fires before parallel subtests run |
| `t.Context()` | Context canceled just before Cleanup runs | Goroutines that outlive the test should watch it |
| `t.TempDir()` | Hermetic dir, removed after the test | Point `HOME` and config-dir env vars at it, never the real one |
| `t.Setenv`, `t.Chdir` | Process-wide env/cwd, restored after | Panics under `t.Parallel()`: keep those serial |
| `t.ArtifactDir()` | Files kept for inspection (`-artifacts` flag) | Not a scratch dir; use `TempDir` for that |
| `t.Output()` | `io.Writer` into the test log, indented like `t.Log` | Hand it to a logger instead of `os.Stderr` |

- Setup helpers return the value and register their own cleanup: `srv := newTestServer(t)`; callers never `Close`.

## Parallelism and the race detector

- `t.Parallel()` only for tests touching no package var, env, cwd or shared fixture file; it pauses the test until
  its serial siblings finish, so a parent's `defer` runs before parallel children — use `t.Cleanup`.
- Run the touched packages under `go test -race` whenever the change involves goroutines, channels or shared state;
  the detector only sees races the run executes, so drive the concurrent path (N goroutines, a barrier) on purpose.
- `-race` slows code 2-20x: scale timeouts from a package var, don't hardcode wall-clock budgets.
- Leak check without a dependency: compare `runtime.NumGoroutine()` before and after with a short poll, or wait on
  the goroutine's own done channel. `goleak` is fine if the module already imports it — don't add it for one test.

## Time and goroutines: `testing/synctest`

- `synctest.Test(t, func(t *testing.T) { … })` runs the body in a bubble with a fake clock that advances only when
  every bubbled goroutine is durably blocked: timers, `time.Sleep`, `context.WithTimeout` become instant and exact.
- `synctest.Wait()` blocks until all other bubbled goroutines are blocked — the sync point before asserting.
- `synctest.Sleep(d)` advances the clock by `d` and then waits, replacing the `time.Sleep` + `Wait` pair.
- Blocking on I/O, a mutex or a channel created outside the bubble is not "durable": the clock stalls. Use
  in-memory pipes (`net.Pipe`) or `httptest.NewTestServer`, which uses an in-memory network.
- Prefer synctest over a clock interface: production keeps calling `time.Now`, and no fake-clock dependency lands.

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

- Read `rec.Result()` (status, headers, body) rather than poking recorder fields; assert the body's shape, not bytes.
- Build requests with `NewRequestWithContext(t.Context(), …)` so a hung handler is canceled at test end.
- An upstream fake records what it received (method, path, headers, body) for assertions; assert sensitive headers
  are absent on error paths.
- Error paths first: malformed body, wrong method, missing auth, upstream 5xx and a mid-body disconnect.

## Golden files

- Fixtures live in `testdata/` (the toolchain ignores it); goldens are `testdata/<name>.golden`.
- One package-level `var update = flag.Bool("update", false, "rewrite golden files")`, declared in the test file of
  the source that owns the output. Regenerate with `go test ./pkg -run TestName -update`, then review the diff.
- On mismatch print a diff and the regenerate command; never auto-update on failure.
- Normalize before compare: sort map-derived output, replace temp paths and timestamps with fixed tokens, `\r\n` to
  `\n` so the golden passes on Windows.

## Fuzzing

- Seed with `f.Add` from real inputs and the regression cases; arguments are limited to `[]byte`, `string`, bools,
  ints, uints, floats and runes.
- Assert properties, not outputs: round-trip (`decode(encode(x)) == x`), no panic, invariants (length, validity),
  agreement with a slow reference implementation.
- A crash writes `testdata/fuzz/FuzzX/<hash>`; commit it — plain `go test` replays the corpus as regression cases.
- `-fuzz` accepts exactly one target per package; bound runs with `-fuzztime 30s` (or `100000x`) outside CI.
- Skip cases that are invalid by construction with `t.Skip()` rather than asserting on garbage.

## Examples

- `ExampleFoo` with `// Output:` is a test (exact stdout, outer whitespace trimmed); `// Unordered output:` for map
  or concurrent output. No output comment means compiled but never run — not a guard.
- Suffix rules: `Example` (package), `ExampleFoo`, `ExampleT_Method`, `ExampleT_Method_variant` (lowercase variant).

## Coverage

- `-covermode=atomic` with `-race`; `-coverpkg=./...` to credit packages exercised from another package's tests.
- An executed line is not an asserted one: use coverage to find branches you meant to test, never as a target.

## Benchmarks

- Write new ones with `for b.Loop() { … }`: setup above the loop is excluded from timing, and the body's calls are
  kept alive, so no `b.ResetTimer` or result sink. Legacy `for range b.N` code may stay as is.
- `b.ReportAllocs()` (or `-benchmem`) for B/op and allocs/op; `b.ReportMetric(v, "unit")` for domain metrics.
- One `b.Run` per variant or input size (`size=64`, `size=4k`) — the sub-name is the row benchstat compares, and a
  size sweep exposes superlinear growth no single size shows.
- `b.SetBytes(n)` adds MB/s; `b.RunParallel` measures contention, not single-op speed.

| Flag | Effect |
| --- | --- |
| `-count=10` | Samples for statistics; under 6 benchstat cannot reach significance |
| `-benchtime=2s` / `500x` | Time or fixed iterations per sample |
| `-cpu=1,4,8` | Repeat at several GOMAXPROCS (`-8` suffix in the name) |
| `-cpuprofile`, `-memprofile`, `-trace` | Profile the benchmark directly; read with `go tool pprof` / `go tool trace` |

## Comparing with benchstat

```bash
cd /path/to/repo
go test -run '^$' -bench 'Parse' -benchmem -count=10 ./internal/parse > old.txt
# apply the change, same machine, same toolchain, nothing else running
go test -run '^$' -bench 'Parse' -benchmem -count=10 ./internal/parse > new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

- Read the median, `± N%` (95% CI; above ~5% the machine is noisy) and `p=`; `~` means no significant change — do
  not claim one. Raise `-count` or cut noise before concluding.
- Compare only runs on one machine and one Go toolchain; a toolchain bump moves baselines by itself.
- Never benchmark variants concurrently — they share CPUs. Build variants in parallel, measure them serially
  against one baseline file.
- `-col /impl` compares sub-benchmark variants within one file; `-filter '.name:Parse'` narrows rows.
- Recording the delta in the commit: [performance.md](performance.md).

## testify (optional)

Use the stdlib unless the target repo already imports testify. When it does:

- `require` for preconditions (it `FailNow`s), `assert` for the checks after; arguments are `(expected, actual)`.
- Build `assert.New(t)` inside each `t.Run` from the subtest's `t` — one bound to the parent attributes every
  subtest failure to the parent while the subtest reports PASS.
- `ErrorIs`/`ErrorAs` over `Equal` on errors; `JSONEq` for JSON, `ElementsMatch` for unordered, `InDelta` for floats.
- `EventuallyWithT` for polling, but prefer synctest when the wait is on timers in your own code.
- `mock.Mock` only at real boundaries (network, clock, external processes), and always `AssertExpectations(t)`; a seam
  var holding a closure is usually shorter. Skip `suite`: `t.Run` plus `t.Cleanup` covers its lifecycle.
- `testifylint` (golangci-lint) catches swapped arguments and assert-for-require misuse.
