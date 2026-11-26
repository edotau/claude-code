# Go Errors and Testing

Harness test layout: [go.md](../../../rules/harness/go.md#tests). Test scope, seams, fixtures and execution:
[testing-guidelines.md](../../../rules/standards/testing-guidelines.md).

## Error boundaries

- Log or return an error, not both. Wrap only when adding context; operation prefixes omit "failed to" and type names.
  Remediation belongs at the user boundary; variable data belongs in fields, never secrets in the error string.
- Export only sentinels/types callers inspect. `%w` exposes a cause as API; translate implementation details at the
  boundary. `%v` removes unwrap access but still leaks the original text.
- `io.Reader.Read` may return bytes and an error together: process bytes first. A typed nil pointer inside `error`
  is non-nil; a shadowed `err` can leave a named return unchanged.
- `fmt.Errorf` with `%w` and nil still returns an error. `errors.Unwrap` does not traverse multi-errors; use `Is`/`As`.
- `errors.AsType[T]` needs `T` to implement `error`; a behavior-only interface still needs `errors.As`.
- Capture unexpected-fault stacks once at their origin, not on every wrap. Keep sentinels immutable.
- Retry classified transient failures only; expected cancellation normally stops quietly. Log only the final failure
  with its attempt count. Domain-to-HTTP/RPC translation belongs at the boundary, with safe client text.

## Deferred cleanup

Capture close errors for files you write; join with an earlier error so cleanup cannot overwrite it with nil.
Read-only handles can use plain deferred close. A shared error variable across workers races: collect separately,
then join; use `errgroup` when the first failure should cancel siblings.

## Panics

Recover only in a deferred function in the panicking goroutine; capture its stack there. Convert panics at library
boundaries when needed; do not recover-and-continue after a broken core invariant (`runtime.Error`). HTTP recovery
must re-panic `http.ErrAbortHandler`. Expected input/I/O failures return errors; `Must*` is for literal init constants.

## Lifecycle helpers on `testing.T`

- A parent's defer runs before parallel subtests; `t.Cleanup` runs after them. `t.Context` cancels before cleanup.
- Process-wide env/cwd or package-var swaps cannot run with `t.Parallel`; restore seams with `t.Cleanup`.
- An example without an output comment compiles but never runs. Fuzz failures become replayable corpus entries;
  normalize nondeterministic golden output and review deliberate updates, never update on failure.
- Stdlib assertions unless the repo already uses testify. Bind `assert.New` to each subtest's `t`, not the parent;
  use `require` for prerequisites, assert mock expectations, and prefer `t.Run`/`t.Cleanup` to suites.

## Parallelism, races and time

Race detection sees only executed paths; exercise actual contention. In `synctest.Test`, time advances only when all
bubbled goroutines durably block; call `synctest.Wait` before asserting. Mutex waits, real I/O and outside channels
do not durably block. Use `net.Pipe` or, where available, `httptest.NewTestServer`'s in-memory network.

## HTTP

Use a recorder for one handler, an owned test server for a client. `NewTestServer(t, h)` uses an in-memory network
and test cleanup; `NewTLSServer` uses a real loopback socket and its `Client` trusts the test certificate.

## Benchmarks

`b.Loop` excludes setup and keeps calls alive without a result sink. Compare the same named variants, workload,
machine and toolchain with allocation counts and at least six samples for `benchstat`. A `~` is no significant
change; reduce noise or collect more evidence before claiming one. Profiling artifacts and execution scope belong
to the rules linked above, not the source tree. Profiling choices: [performance.md](performance.md#measure-first).
