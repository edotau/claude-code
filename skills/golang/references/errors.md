# Go Error Handling

Creating, wrapping, inspecting, logging and surfacing errors in Go 1.22+ code (module targets Go 1.27).

## Core rules

- Return `error` as the last result and check it on the next line — a later check hides which call failed.
- Read the other results only after the check: `v, err := f(); use(v)` before `if err != nil` uses garbage.
- Never discard an error with `_` without a comment saying why it can't matter — silent drops are the worst to find.
- Handle an error exactly once: log it **or** return it, never both — double handling floods logs with one failure.
- On error, return zero values for the other results; callers must not read them — partial results invite misuse.
- Exception: `io.Reader.Read` and similar may return `n > 0` with an error; process the bytes before the error.
- Fail fast with early returns; keep the happy path at the left margin (nesting ≤ 4).
- Never return a typed nil pointer as `error` — the interface is non-nil and `err != nil` is true.

```go
func find() error {
    var e *NotFoundError // nil pointer
    return e             // WRONG: non-nil error interface; return nil explicitly
}
```

## Message style

- Lowercase, no trailing punctuation, no newlines — messages get concatenated by wrapping (`open config: read: EOF`).
- Describe the operation, not the failure: `"load config: %w"`, not `"failed to load config: %w"` — every layer
  saying "failed to" adds noise and no information.
- No package or type prefix (`"UserService: ..."`) — the wrap chain already says where; prefixes duplicate it.
- State what happened, not what to do about it: remediation hints belong to the layer that talks to the user.
- Add context the caller doesn't already have (the path, the ID, the key); don't repeat the function's own arguments
  that the caller passed in and already knows.
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

- Sentinels are exported `Err*` package vars; custom types are `*Error`-suffixed. Both become public API — export
  only what callers genuinely branch on.
- Give a custom type a pointer receiver for `Error()` and return it as a pointer, so `As` targets are consistent.
- Implement `Unwrap() error` on a custom type that carries a cause, or `Is`/`As` stop at it.
- Implement `Is(target error) bool` only when equality is semantic (e.g. matching by code), not to fake a hierarchy.
- Prefer behaviour checks over type checks across package boundaries: an interface like
  `interface{ Temporary() bool }` asserted with `errors.As` couples less than a concrete type.
- Sentinels are allocated once at package init; returning one costs nothing, so hot paths prefer them to `fmt.Errorf`.

```go
type NotFoundError struct{ Kind, ID string }

func (e *NotFoundError) Error() string { return e.Kind + " " + e.ID + ": not found" }
func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound } // also matches the sentinel
```

## Wrapping

- `%w` wraps (callers can inspect the cause); `%v` formats only (cause is opaque). Choose deliberately.
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
- `errors.Is(err, ErrX)` walks the whole chain (including `Join` and multi-`%w` trees).
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
- Use it for validation (report every bad field), cleanup that must keep going, and fan-out results.
- In concurrent code, collect per-goroutine errors into a slice indexed by worker (no shared mutable error), then
  join; use `errgroup` when the first error should cancel the rest.

```go
func (c Config) Validate() error {
    var errs []error
    if c.Name == "" {
        errs = append(errs, errors.New("name: required"))
    }
    if c.Port <= 0 {
        errs = append(errs, fmt.Errorf("port %d: must be positive", c.Port))
    }
    return errors.Join(errs...)
}
```

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
- Log with structured attributes: `slog.Error("sync failed", "err", err, "repo", id)` — the `err` attribute keeps
  the full chain; variable data stays in fields for grouping. Use `slog`, not `fmt.Println` or `log.Printf`.
- The log message is a constant, never `err.Error()` — a dynamic message makes every failure its own group.
- Give a field-carrying error type a `LogValue() slog.Value` (`slog.LogValuer`) so its fields log as attributes
  wherever it surfaces; request-scoped data (trace ID, user) comes from `ctx` at the log site, not from every wrap.
- Logging an attempt you then retry is handling it; return only the final failure, wrapped with the attempt count.
- Choose level by who must act: client errors (bad input, not found) at Info/Warn or not at all; server faults at Error.
- At an HTTP/RPC boundary, map domain errors to status codes in one place, and send the client a safe message —
  never the raw wrapped chain, which leaks internals. A type that needs a user-facing message carries it in its own
  field, extracted with `AsType` at the boundary, with a generic fallback for everything else.

```go
func status(err error) int {
    switch {
    case errors.Is(err, ErrNotFound):
        return http.StatusNotFound
    case errors.Is(err, ErrConflict):
        return http.StatusConflict
    case errors.Is(err, context.DeadlineExceeded):
        return http.StatusGatewayTimeout
    default:
        return http.StatusInternalServerError
    }
}
```

- CLI: print one line to stderr and exit non-zero from `main`; user-facing wording rules are in
  [cli-text.md](../../../rules/software/go/cli-text.md).

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
- [ ] No `==`, type assertion or string matching on errors; `Is`/`AsType` throughout.
- [ ] Log messages are constants; variable data in attributes; logged via `slog`.
- [ ] Written files capture `Close` errors; deferred cleanup doesn't clobber `err`.
- [ ] No panics for expected failures; spawned goroutines that must survive recover.
- [ ] No secrets or PII in messages; boundary responses don't leak the chain.

Testing error paths (assert with `errors.Is`/`AsType`, not message text; error paths first): see
[testing.md](../../../rules/software/go/testing.md) and
[testing-guidelines.md](../../../rules/standards/testing-guidelines.md).

Adapted from samber/cc-skills-golang (MIT): golang-error-handling.
