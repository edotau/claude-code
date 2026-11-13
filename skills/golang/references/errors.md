# Go Error Handling

Creating, wrapping, inspecting, logging and surfacing errors in Go 1.22+ code.

## Core rules

- Never discard an error with `_` without a comment saying why it can't matter.
- Handle an error once: log it **or** return it, never both.
- `io.Reader.Read` may return `n > 0` with an error; process the bytes first.
- Never return a typed nil pointer as `error`: the interface is non-nil.

```go
func find() error {
    var e *NotFoundError // nil pointer
    return e             // WRONG: non-nil error interface; return nil explicitly
}
```

## Message style

- Describe the operation, not the failure: `"load config: %w"`, not `"failed to load config: %w"`.
- No package or type prefix (`"UserService: ..."`); the wrap chain says where.
- State what happened, not what to do; remediation belongs to the layer that talks to the user.
- Add context the caller lacks (path, ID, key); don't repeat arguments it passed.
- Keep messages low-cardinality for metrics/grouping: variable data goes in structured attributes or error fields. A
  static wrap prefix (`"fetch user: %w"`) is already low-cardinality.
- Data known only deep in the stack: carry it as fields on a custom type (see Logging), not only interpolated text.
- Never put secrets, tokens or PII in an error string.

## Creating errors — decision table

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

```go
type NotFoundError struct{ Kind, ID string }

func (e *NotFoundError) Error() string { return e.Kind + " " + e.ID + ": not found" }
func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound } // also matches the sentinel
```

## Wrapping

- `%w` inside your module; at a package/API boundary use `%v` (or translate) when exposing the cause would freeze an
  implementation detail (a `*pq.Error` leaking through a repository).
- Multiple `%w` (1.20+) expose all via `Unwrap() []error`: `fmt.Errorf("sync %s: %w: %w", id, ErrConflict, err)`.
- Wrap once per layer that adds information, at least per package boundary; otherwise return `err` unchanged.
- `%v` still copies the cause's text. To hide internals, translate to your own sentinel or type
  (`fmt.Errorf("get user %s: %w", id, ErrUnavailable)`) and log the original.
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

## Aggregating

- `errors.Join` drops nil arguments and returns nil when all are nil: safe to accumulate unconditionally.
- Text is members joined by newlines; prefix-wrap it for single-line logs.
- Concurrent code: per-worker error slice (no shared mutable error), then join; `errgroup` when the first error
  should cancel the rest.

## Deferred cleanup

- `defer f.Close()` on a file you **wrote** loses the write error (buffered data flushes at close). Capture it.

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

- Read-only handles: plain `defer Close()` is fine.
- A deferred cleanup must not overwrite an earlier error with nil; `Join` keeps both.

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
  the raw chain. A type needing a user-facing message carries it in a field, extracted with `AsType`, generic
  fallback otherwise.
- CLI: one line to stderr, non-zero exit from `main`; code mapping: [cli.md](cli.md).

## Gotchas

- Shadowing: `if x, err := f(); err != nil` leaves the outer `err` untouched (a named return may still be nil).
- `err` reused across calls with no check between silently loses the first failure.
- Never mutate or reassign an `Err*` var.
- Retry only classified transient errors (`errors.Is` / behaviour interface); retrying validation or auth errors just
  delays the failure.
- `errors.Is(nil, ErrX)` is false and `errors.Join()` is nil: no special-casing.
- stdlib errors carry no stack. For an unexpected fault capture `runtime.Callers` in a custom type at the origin, not
  per wrap; expected errors need none.

## Review checklist

- [ ] No ignored errors without a justifying comment; no log-and-return.
- [ ] Every wrap adds context and uses `%w` unless deliberately opaque at a boundary.
- [ ] No `==`, type assertion or string matching on errors.
- [ ] Log messages are constants; variable data in attributes.
- [ ] Written files capture `Close` errors; deferred cleanup doesn't clobber `err`.
- [ ] No panics for expected failures; spawned goroutines that must survive recover.
- [ ] No secrets or PII in messages; boundary responses don't leak the chain.

Testing error paths (assert with `errors.Is`/`AsType`, not message text; error paths first): [testing.md](testing.md).
