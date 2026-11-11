# Go Services: HTTP, SQL, gRPC

Serving and calling over `net/http`, `database/sql` and gRPC in production Go (module targets Go 1.27).

Stdlib first, minimal pinned deps: [security.md](../../../rules/standards/security.md) wins over any library
suggestion here. Request-context rules: [concurrency.md](concurrency.md) → context in HTTP services; error wrapping:
[errors.md](errors.md); `httptest`/fakes: [testing.md](testing.md).

## net/http servers

- Go 1.22 `ServeMux` patterns: `"GET /items/{id}"`, `r.PathValue("id")`, `{rest...}` for a tail, `{$}` for an exact
  `/`. `GET` also serves `HEAD`; a path match with the wrong method gets an automatic 405 plus `Allow`.
- Most specific pattern wins; two patterns that overlap with no winner panic at `Handle` time — register in `main`
  (or a test) so the panic fires at startup, not on a live request.
- `r.Pattern` (1.23) is the matched template: the metrics/log label, never `r.URL.Path` (unbounded, leaks IDs).
- Never serve with `http.ListenAndServe` or a zero `http.Server`: every timeout is off and one slow client holds a
  goroutine and a socket forever.

| Field | Bounds | Typical |
| --- | --- | --- |
| `ReadHeaderTimeout` | Headers arriving (Slowloris; gosec G112) | 5s |
| `ReadTimeout` | Whole request incl. body; 0 falls back to `ReadHeaderTimeout` only for headers | 15–30s |
| `WriteTimeout` | From end of header read to end of response write — caps handler run time | 30s; 0 for streaming |
| `IdleTimeout` | Keep-alive gap between requests | 60–120s |

- Streaming or long handlers: keep server-wide timeouts tight and lift them per request with
  `http.NewResponseController(w).SetWriteDeadline(...)` instead of zeroing `WriteTimeout` for everyone.
- Cap bodies at the boundary: `r.Body = http.MaxBytesReader(w, r.Body, n)`; decode with `json.Decoder` and reject
  trailing data (`dec.More()`), and unknown fields where the contract is strict.

### Graceful shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
    BaseContext: func(net.Listener) context.Context { return ctx }}
errc := make(chan error, 1)
go func() { errc <- srv.ListenAndServe() }()
select {
case err := <-errc:
    return err // bind failure etc.; never ErrServerClosed here
case <-ctx.Done():
}
shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()
return srv.Shutdown(shutCtx) // wait on this, not on ListenAndServe: that returns ErrServerClosed at once
```

- `Shutdown` stops accepting and waits for in-flight handlers, but not hijacked connections (websockets): close
  those via `srv.RegisterOnShutdown`. On timeout it returns `ctx.Err()`; follow with `srv.Close()`.
- Behind a load balancer, fail readiness first and wait one probe interval before `Shutdown`, or new requests land
  on a closing listener. Shutdown order: HTTP server → background workers → DB pool / gRPC conns.

### Middleware

- Shape: `func(http.Handler) http.Handler`, composed in `main`; the outermost wrapper runs first. Order: recover →
  request ID/trace → logging/metrics → auth → per-route timeout → handler.
- A `ResponseWriter` wrapper that records status must implement `Unwrap() http.ResponseWriter`, or
  `ResponseController` (flush, deadlines, hijack) silently stops working through it.
- net/http recovers handler panics itself but only logs and drops the connection; own recovery to return a 500 and
  log with request context. Re-panic `http.ErrAbortHandler` — it is the deliberate abort signal.
- Per-request budget: `http.TimeoutHandler` or `WithTimeout(r.Context(), d)` in middleware; CSRF:
  `http.CrossOriginProtection` (1.25) before any library.

### http.Client

- One `*http.Client` per upstream, built once and shared (it is goroutine-safe); a client or `Transport` per request
  defeats connection pooling and leaks idle sockets.
- `http.DefaultClient` has no timeout. Set `Client.Timeout` as the whole-exchange cap (dial → body read) and pass
  `http.NewRequestWithContext(ctx, ...)` for the caller's budget; the earlier one wins.
- Start from `http.DefaultTransport.(*http.Transport).Clone()`; raise `MaxIdleConnsPerHost` (default 2) for a hot
  single upstream, or every burst re-dials.
- Always `defer resp.Body.Close()`; to reuse the connection, read the body to EOF first (`io.Copy(io.Discard, ...)`,
  bounded with `io.LimitReader`).
- A 4xx/5xx is `err == nil`: check `resp.StatusCode` explicitly and turn it into a typed error with the status.
- Retries only for idempotent methods or with an idempotency key, with capped jittered backoff, inside ctx.

## database/sql

- `sql.Open` builds a pool without connecting: `PingContext` once at startup. One `*sql.DB` per database, for life.

| Setter | Default | Rule |
| --- | --- | --- |
| `SetMaxOpenConns` | 0 = unlimited | Always set; sum across all replicas must fit the server's `max_connections` |
| `SetMaxIdleConns` | 2 | ~MaxOpen for steady load; clamped to MaxOpen |
| `SetConnMaxLifetime` | forever | Below any LB/proxy/server idle cut-off; recycles after failover/DNS moves |
| `SetConnMaxIdleTime` | forever | Shed burst connections, e.g. 1–5 min |

- Watch `db.Stats()`: a rising `WaitCount`/`WaitDuration` means pool exhaustion — fix slow queries or leaks before
  raising `MaxOpenConns`.
- Context everywhere: only the `*Context` methods (`QueryContext`, `ExecContext`, `QueryRowContext`, `BeginTx`).
  The context-free forms keep running after the client is gone and hold a connection while they do.
- `Exec` for statements without rows. `Query` returns `*Rows` that pin a connection until closed — using it for an
  `UPDATE` leaks one connection per call.
- Placeholders are driver syntax (`$1` pgx/lib/pq, `?` MySQL/SQLite); never build SQL with `fmt.Sprintf` from input.
  Dynamic identifiers (sort column) come from an allowlist map, never the request.

```go
rows, err := db.QueryContext(ctx, `SELECT id, name, deleted_at FROM users WHERE org = $1`, org)
if err != nil {
    return nil, fmt.Errorf("query users: %w", err)
}
defer rows.Close() // releases the connection on every path, including a Scan error
var out []User
for rows.Next() {
    var u User
    if err := rows.Scan(&u.ID, &u.Name, &u.DeletedAt); err != nil {
        return nil, fmt.Errorf("scan user: %w", err)
    }
    out = append(out, u)
}
return out, rows.Err() // errors mid-iteration (network, cancel) surface only here
```

- `QueryRowContext(...).Scan` reports a miss as `sql.ErrNoRows`: map it with `errors.Is` to a domain
  `ErrNotFound` or `(v, false, nil)`; everything else is wrapped and returned.
- Driver errors (unique violation, serialization failure) are typed per driver (`*pgconn.PgError` `.Code`):
  inspect with `errors.As` in the repository layer and translate there, so callers never import the driver.

### Transactions

```go
func withTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
    tx, err := db.BeginTx(ctx, opts)
    if err != nil {
        return fmt.Errorf("begin: %w", err)
    }
    defer tx.Rollback() // after Commit it returns ErrTxDone: harmless, deliberately ignored
    if err := fn(tx); err != nil {
        return err
    }
    return tx.Commit()
}
```

- Inside `fn` use only `tx`; a stray `db.` call runs on a second connection outside the transaction and can
  deadlock against it when the pool is small.
- Read-modify-write: `SELECT ... FOR UPDATE` (or `NOWAIT`/`SKIP LOCKED` for queues), or `LevelSerializable` and
  retry the whole `withTx` on SQLSTATE `40001`/`40P01` — never retry a single statement inside the failed tx.
- A tx is one connection (close one `Rows` before the next query); cancelling its ctx rolls it back. Keep it short:
  no network calls or user waits inside — locks are held until it ends.
- Batch writes in chunks (hundreds to low thousands of rows); Postgres bulk loads use COPY (`pgx.CopyFrom`).

### NULLs

| Option | Use when |
| --- | --- |
| `sql.Null[T]` (1.22) | Explicit validity in domain code; one generic type instead of `NullString`, `NullInt64`, ... |
| `*T` field | The value round-trips to JSON as `null`; nil is the natural "absent" |
| `COALESCE(col, '')` in SQL | NULL and zero mean the same thing to every caller |

Scanning a NULL into a plain `string`/`int64` is a runtime error, not a zero value — decide per nullable column.

### sqlc vs query builders vs ORM

| Choice | Pick when | Cost |
| --- | --- | --- |
| `database/sql` + driver | Few queries, portability, zero extra deps | Hand-written `Scan` lists |
| sqlc | SQL-first, many queries: generates typed Go from `.sql`, checked against the schema | Build step |
| pgx native (`pgxpool`) | Postgres-only: COPY, LISTEN/NOTIFY, arrays, faster wire protocol | Not `database/sql` |
| sqlx | Struct scanning and named args over `database/sql` | One small dep, runtime reflection |
| GORM / ent | Rarely: hidden queries, N+1 by default, hooks run implicitly | Debugging the generated SQL |

Default to sqlc (or plain `database/sql`) in new code. Migrations go through an external tool (golang-migrate, Atlas,
goose) as reviewed, versioned SQL — the application does not mutate its own schema at startup.

## gRPC

- `grpc.NewClient` (lazy; `Dial`/`DialContext` are deprecated) once per target and share the `*ClientConn`: HTTP/2
  multiplexes all RPCs over it. Use `dns:///host:port` plus `round_robin` against headless services.
- Server types embed `pb.UnimplementedXServer` by value (not `Unsafe*`) so new RPCs compile and return
  `Unimplemented`. Keep handlers thin: decode → call domain service → map errors to status.
- Register `grpc_health_v1`; leave reflection off in production. `GracefulStop` in a goroutine with a timeout
  falling back to `Stop` — a long stream otherwise blocks shutdown forever.
- Messages cap at 4 MB received by default (`grpc.MaxRecvMsgSize`); prefer streaming to raising it. Client keepalive
  pings faster than the server's `EnforcementPolicy.MinTime` get the connection killed with `too_many_pings`.

### Interceptors

- `grpc.ChainUnaryInterceptor(a, b, c)` / `ChainStreamInterceptor`: `a` is outermost. Order: recovery → tracing →
  logging/metrics → auth → validation. Register stream twins too, or every streaming RPC skips auth; a stream
  interceptor that injects values wraps `grpc.ServerStream` and overrides `Context()`.
- Metadata: `metadata.FromIncomingContext` on the server; `metadata.AppendToOutgoingContext` on the client. Incoming
  metadata is not forwarded downstream automatically — copy only the keys you mean to propagate.

```go
func logUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
    start := time.Now()
    resp, err := h(ctx, req)
    slog.InfoContext(ctx, "rpc", "method", info.FullMethod, "code", status.Code(err), "dur", time.Since(start))
    return resp, err
}
```

### Deadlines

- Every client call carries a deadline (`context.WithTimeout`, or a service-config `methodConfig.timeout` default);
  no deadline means a hung upstream pins the caller's goroutine indefinitely.
- The deadline travels on the wire (`grpc-timeout`); a server passing its handler `ctx` to downstream calls
  propagates the remaining budget for free. Never swap in `context.Background()` mid-chain.
- Long server work checks `ctx.Err()` between steps; the client already got `DeadlineExceeded` or `Canceled`.

### Status codes and details

| Code | Meaning | Client should |
| --- | --- | --- |
| `InvalidArgument` / `OutOfRange` | Bad input regardless of state | Fix the request, never retry |
| `NotFound` / `AlreadyExists` | Entity missing / duplicate on create | Act on it |
| `FailedPrecondition` | State forbids it now (not empty, wrong phase) | Change state first, not retry |
| `Aborted` | Concurrency conflict (tx abort, version clash) | Retry the read-modify-write |
| `Unauthenticated` / `PermissionDenied` | No valid credentials / not allowed | Re-auth / give up |
| `ResourceExhausted` | Quota, rate limit, message too large | Back off |
| `Unavailable` | Transient; the only safe automatic retry | Retry with backoff |
| `DeadlineExceeded` | Budget spent; may have run | Retry only if idempotent |
| `Internal` / `Unknown` | Bug or unmapped error | Alert |

- Return `status.Error(codes.X, msg)` from handlers: a plain `error` becomes `Unknown` and breaks client retry
  logic. Map with `errors.Is`/`errors.As` at the handler edge.
- Never pass a downstream status straight through — its `NotFound` is not your `NotFound`; re-map at your boundary.
- `Internal` messages go to the caller: log the detail server-side and return a generic message.
- Structured details: `st, _ := status.New(codes.InvalidArgument, "bad").WithDetails(&errdetails.BadRequest{...})`
  then `st.Err()`; clients read them with `status.FromError(err)` → `st.Details()` and type-switch.

### Streaming lifecycle

| Side | Ends the stream by | Leak if you forget |
| --- | --- | --- |
| Server handler | Returning: `nil` → `OK`, a status error → that code | n/a — return is the end |
| Client, server-stream | `Recv` until `io.EOF` (clean) or another error | Stream and goroutine stay until ctx ends |
| Client, client-stream | `CloseAndRecv` after the last `Send` | Server waits for input forever |
| Client, bidi | `CloseSend` for the half-close, then drain `Recv` | Same; always also `defer cancel()` the ctx |

- One goroutine may `Send` while another `Recv`s; two concurrent `Send`s (or `Recv`s) on one stream are a race.
- `Send` blocks on flow control when the peer is slow — select on `stream.Context().Done()` in producer loops.
- A per-call deadline bounds the whole stream: long-lived streams use cancellation plus keepalive, not a timeout.

## Picking libraries

- Routing: `net/http` `ServeMux` covers methods, wildcards and precedence since 1.22. chi is the one router worth a dep
  (stays `http.Handler`-native, for route groups); avoid fasthttp-based frameworks (fiber) — not `net/http`.
- HTTP clients: plain `http.Client` (resty/req add deps for little); SQL: driver (pgx, go-sql-driver/mysql,
  modernc.org/sqlite for cgo-free) + sqlc, per the table above.
- GraphQL: gqlgen (schema-first, codegen) only when a GraphQL contract is required; enforce complexity limits and
  per-request DataLoaders.
- OpenAPI: generate from a spec (oapi-codegen) over comment annotations (swaggo) — the spec stays the contract.
- Before adding any of these: check stdlib, pin the version, run `govulncheck`; prefer one dep per concern.

Adapted from samber/cc-skills-golang (MIT): golang-database, golang-grpc, golang-graphql, golang-swagger,
golang-popular-libraries.
