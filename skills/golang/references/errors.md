# Go Error Handling

Creating, wrapping, inspecting, logging and surfacing errors in Go 1.22+ code.

## Core rules

- Never discard an error with `_` without a comment saying why it can't matter.
- Handle an error exactly once: log it **or** return it, never both — double handling floods logs with one failure.
- `io.Reader.Read` and similar may return `n > 0` with an error; process the bytes before the error.
- Never return a typed nil pointer as `error` — the interface is non-nil and `err != nil` is true.

```go
func find() error {
    var e *NotFoundError // nil pointer
    return e             // WRONG: non-nil error interface; return nil explicitly
}
```

## Message style

- Describe the operation, not the failure: `"load config: %w"`, not `"failed to load config: %w"` — every layer
  saying "failed to" adds noise and no information.
- No package or type prefix (`"UserService: ..."`) — the wrap chain already says where.
- State what happened, not what to do about it: remediation hints belong to the layer that talks to the user.
- Add context the caller doesn't already have (the path, the ID, the key); don't repeat arguments the caller passed.
- Keep messages low-cardinality when they feed metrics or grouping: put variable data in structured log attributes or
  error fields, not only in the string. A static wrap prefix (`"fetch user: %w"`) is already low-cardinality.
- Variable data known only deep in the stack is lost by the time the top logs it: carry it as fields on a custom
  type (see Logging) rather than only interpolating it into text.
- Never put secrets, tokens or PII in an error string — errors end up in logs, responses and crash reports.

## Creating errors — decision table

| Need | Use | Caller checks with |
| --- | --- | --- |
| Fixed condition callers branch on | sentinel: `var ErrNotFound = errors.New("not found")` | `errors.Is` |
| Condition with data callers read | custom type: `type ValidationError struct{ Field string }` | `errors.AsType` |
| One-off, nobody branches on it | `errors.New` / `fmt.Errorf` inline | nothing — message only |
| Add context to an underlying error | `fmt.Errorf("op %s: %w", id, err)` | `Is`/`As` see through it |
| Several independent failures | `errors.Join(errs...)` | `Is`/`As` match any member |

- Sentinels (`Err*`) and custom types (`*Error`) become public API — export only what callers genuinely branch on.
- Give a custom type a pointer receiver for `Error()` and return it as a pointer, so `As` targets are consistent.
- Implement `Unwrap() error` on a custom type that carries a cause, or `Is`/`As` stop at it.
- Implement `Is(target error) bool` only when equality is semantic (e.g. matching by code), not to fake a hierarchy.
- Prefer behaviour checks over type checks across package boundaries: an interface like
  `interface{ Temporary() bool }` asserted with `errors.As` couples less than a concrete type.
- Hot paths: sentinels over `fmt.Errorf` ([performance.md](performance.md)).

```go
type NotFoundError struct{ Kind, ID string }

func (e *NotFoundError) Error() string { return e.Kind + " " + e.ID + ": not found" }
func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound } // also matches the sentinel
```

## Wrapping

- Wrap with `%w` inside your module; at a package or API boundary, use `%v` (or translate) when exposing the cause
  would freeze an implementation detail into your contract — e.g. a `*pq.Error` leaking through a repository.
- Multiple `%w` verbs in one `fmt.Errorf` (Go 1.20+) produce an error whose `Unwrap() []error` exposes all of them:
  `fmt.Errorf("sync %s: %w: %w", id, ErrConflict, err)` — a domain sentinel plus the raw cause.
- Wrap once per layer that adds information — at least at each package boundary, not at every call; a layer with
  nothing to add returns `err` unchanged.
- `%v` hides the type but still copies the cause's text. To keep internals out of a public API, translate: map the
  cause to your own sentinel or type (`fmt.Errorf("get user %s: %w", id, ErrUnavailable)`) and log the original.
- Don't wrap `nil`: `fmt.Errorf("x: %w", nil)` returns a non-nil error with text `x: %!w(<nil>)`.

## Inspecting

- Never compare with `==`, type-assert (`err.(*T)`) or match on `err.Error()` text — all break once anything wraps it.
  `errors.Is` walks the whole chain, including `Join` and multi-`%w` trees.
- Go 1.26+: `errors.AsType[T](err) (T, bool)` replaces the pointer-target dance and can't panic on a bad target.

```go
if nf, ok := errors.AsType[*NotFoundError](err); ok {
    return notFound(nf.Kind, nf.ID)
}
// pre-1.26: var nf *NotFoundError; if errors.As(err, &nf) { ... }  — target must be a non-nil pointer
```

- `AsType`'s `T` must implement `error`; a behaviour interface like `interface{ Temporary() bool }` still needs
  `errors.As` with a pointer to an interface variable.
- `errors.Unwrap` returns nil for multi-errors (it only calls `Unwrap() error`); use `Is`/`As`, not manual unwrapping.
- Check `context.Canceled` / `context.DeadlineExceeded` with `errors.Is`, and usually stop quietly — the caller
  already knows it gave up. `context.Cause(ctx)` gives the reason passed to `WithCancelCause`.

## Aggregating

- `errors.Join` drops nil arguments and returns nil when all are nil — safe to accumulate unconditionally.
- Its text is members joined by newlines; wrap the join with a one-line prefix if it lands in a single-line log.
- In concurrent code, collect per-goroutine errors into a slice indexed by worker (no shared mutable error), then
  join; use `errgroup` when the first error should cancel the rest.

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

- For read-only handles a plain `defer Close()` is fine; the close error carries no lost data.
- A deferred cleanup must not overwrite an earlier error with nil — `Join` keeps both.

## Panics

| Situation | Do |
| --- | --- |
| Expected failure (I/O, input, network, not found) | return an `error` |
| Programmer bug / broken invariant (impossible switch arm, nil required dep) | `panic` with a clear message |
| `Must*` helper for init-time constants (`regexp.MustCompile`) | panic is fine — input is a literal |
| Goroutine you spawn | `recover` at its top if the process must survive — any unrecovered panic kills it |
| Library API | never let a panic cross the public boundary; convert to `error` |

- `recover` only works in a function deferred directly by the panicking goroutine.
- Converting a recovered value: if `r` is an `error`, wrap it with `%w` so callers can inspect it, and capture
  `debug.Stack()` inside the deferred function — the stack is gone once it returns.
- Don't recover-and-continue on `runtime.Error` in core state code; the invariant is already broken.
- HTTP recovery middleware: log the value and stack, answer 500 with a generic body, and re-panic
  `http.ErrAbortHandler` — it is net/http's deliberate abort, not a bug.

## Logging and surfacing

- Log at the top of the call stack (handler, `main`, worker loop) where the error stops propagating — lower layers
  wrap and return.
- `slog.Error("sync failed", "err", err, "repo", id)`: the message is a constant, never `err.Error()` — a dynamic
  message makes every failure its own group; variable data stays in attributes.
- Give a field-carrying error type a `LogValue() slog.Value` (`slog.LogValuer`) so its fields log as attributes
  wherever it surfaces; request-scoped data (trace ID, user) comes from `ctx` at the log site, not from every wrap.
- Logging an attempt you then retry is handling it; return only the final failure, wrapped with the attempt count.
- Choose level by who must act: client errors (bad input, not found) at Info/Warn or not at all; server faults at Error.
- At an HTTP/RPC boundary, map domain errors to status codes in one place (an `errors.Is` switch), and send the
  client a safe message — never the raw wrapped chain, which leaks internals. A type that needs a user-facing message
  carries it in its own field, extracted with `AsType` at the boundary, with a generic fallback for everything else.
- CLI: print one line to stderr and exit non-zero from `main`; exit-code mapping is in [cli.md](cli.md).

## Gotchas

- Shadowing: `if x, err := f(); err != nil` inside a block leaves the outer `err` untouched — a named return set later
  may still be nil.
- `err` reused across sequential calls without a check in between silently loses the first failure.
- A sentinel built with `fmt.Errorf` at package level is fine, but never mutate or reassign an `Err*` var.
- Retrying: only retry errors you've classified as transient (`errors.Is` / a behaviour interface); retrying
  validation or auth errors just delays the failure.
- `errors.Is(nil, ErrX)` is false and `errors.Join()` is nil — no special-casing needed.
- Stack traces: stdlib errors carry none. Need one for an unexpected fault? Capture `runtime.Callers` in a custom
  type at the origin, not per wrap; expected errors (not found, invalid input) don't need one.

## Review checklist

- [ ] No ignored errors without a justifying comment; no log-and-return.
- [ ] Every wrap adds context and uses `%w` unless deliberately opaque at a boundary.
- [ ] No `==`, type assertion or string matching on errors.
- [ ] Log messages are constants; variable data in attributes.
- [ ] Written files capture `Close` errors; deferred cleanup doesn't clobber `err`.
- [ ] No panics for expected failures; spawned goroutines that must survive recover.
- [ ] No secrets or PII in messages; boundary responses don't leak the chain.

Testing error paths (assert with `errors.Is`/`AsType`, not message text; error paths first): see
[testing.md](testing.md).
