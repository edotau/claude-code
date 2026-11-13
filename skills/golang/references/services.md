# Go Services: HTTP, SQL, gRPC

Production `net/http`, `database/sql` and gRPC. Related: [concurrency.md](concurrency.md) (request context),
[errors.md](errors.md), [testing.md](testing.md) (`httptest`), [security.md](security.md) (injection, TLS, CSRF).

## net/http servers

- Go 1.22 `ServeMux` patterns: `"GET /items/{id}"`, `r.PathValue("id")`, `{rest...}` for a tail, `{$}` for an exact
  `/`. `GET` also serves `HEAD`; a path match with the wrong method gets an automatic 405 plus `Allow`.
- Most specific pattern wins; overlapping patterns with no winner panic at `Handle` time, so register in `main` (or a
  test) and the panic fires at startup.
- `r.Pattern` (1.23) is the matched template: use it as the metrics/log label, never `r.URL.Path`.
- Never serve with `http.ListenAndServe` or a zero `http.Server`: every timeout is off.

| Field | Bounds | Typical |
| --- | --- | --- |
| `ReadHeaderTimeout` | Headers arriving (Slowloris; gosec G112) | 5s |
| `ReadTimeout` | Whole request incl. body; 0 falls back to `ReadHeaderTimeout` only for headers | 15–30s |
| `WriteTimeout` | End of header read to end of response write; caps handler run time | 30s; 0 for streaming |
| `IdleTimeout` | Keep-alive gap between requests | 60–120s |

- Streaming/long handlers: keep server timeouts tight and lift per request with
  `http.NewResponseController(w).SetWriteDeadline(...)`.
- Cap bodies at the boundary: `r.Body = http.MaxBytesReader(w, r.Body, n)`; decode with `json.Decoder`, reject
  trailing data (`dec.More()`) and unknown fields where strict.

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

- `Shutdown` waits for in-flight handlers, not hijacked connections (websockets): close those via
  `srv.RegisterOnShutdown`. On timeout it returns `ctx.Err()`; follow with `srv.Close()`.
- Behind a load balancer, fail readiness first and wait one probe interval before `Shutdown`. Order: HTTP server →
  background workers → DB pool / gRPC conns.

### Middleware

- Order, outermost first: recover → request ID/trace → logging/metrics → auth → per-route timeout → handler.
- A status-recording `ResponseWriter` wrapper must implement `Unwrap() http.ResponseWriter`, or `ResponseController`
  (flush, deadlines, hijack) silently stops working.
- net/http recovers handler panics but only logs and drops the connection; own recovery to return a 500 with request
  context. Re-panic `http.ErrAbortHandler` (the deliberate abort signal).

### http.Client

- One shared `*http.Client` per upstream (goroutine-safe); a client or `Transport` per request defeats pooling and
  leaks idle sockets.
- `http.DefaultClient` has no timeout. Set `Client.Timeout` (dial → body read) and use
  `http.NewRequestWithContext(ctx, ...)`; the earlier deadline wins.
- Start from `http.DefaultTransport.(*http.Transport).Clone()`; raise `MaxIdleConnsPerHost` (default 2) for a hot
  upstream.
- A 4xx/5xx is `err == nil`: check `resp.StatusCode` and return a typed error.

## database/sql

- `sql.Open` builds a pool without connecting: `PingContext` once at startup. One `*sql.DB` per database, for life.

| Setter | Default | Rule |
| --- | --- | --- |
| `SetMaxOpenConns` | 0 = unlimited | Always set; sum across replicas must fit the server's `max_connections` |
| `SetMaxIdleConns` | 2 | ~MaxOpen for steady load; clamped to MaxOpen |
| `SetConnMaxLifetime` | forever | Below any LB/proxy/server idle cut-off; recycles after failover/DNS moves |
| `SetConnMaxIdleTime` | forever | Shed burst connections, e.g. 1–5 min |

- Only `*Context` methods (`QueryContext`, `ExecContext`, `QueryRowContext`, `BeginTx`): the plain forms keep running
  and hold a connection after the client is gone.
- Placeholders and dynamic identifiers: [security.md](security.md) → Injection.
- `defer rows.Close()` right after the error check; return `rows.Err()` after the loop (mid-iteration errors surface
  only there).
- `QueryRowContext(...).Scan` reports a miss as `sql.ErrNoRows`: map with `errors.Is` to a domain `ErrNotFound` or
  `(v, false, nil)`.
- Driver errors (unique violation, serialization failure) are typed per driver (`*pgconn.PgError` `.Code`): translate
  with `errors.As` in the repository layer so callers never import the driver.

### Transactions

- Wrap in `withTx(ctx, db, opts, fn)`: `BeginTx`, `defer tx.Rollback()` (after `Commit` it returns `ErrTxDone`,
  deliberately ignored), `fn(tx)`, `Commit`.
- Inside `fn` use only `tx`; a stray `db.` call runs on a second connection outside the tx and can deadlock a small
  pool.
- Read-modify-write: `SELECT ... FOR UPDATE` (`NOWAIT`/`SKIP LOCKED` for queues), or `LevelSerializable` and retry
  the whole `withTx` on SQLSTATE `40001`/`40P01`, never a single statement inside the failed tx.
- A tx is one connection (close one `Rows` before the next query); cancelling its ctx rolls back. Keep it short: no
  network calls or user waits while locks are held.

### NULLs

| Option | Use when |
| --- | --- |
| `sql.Null[T]` (1.22) | Explicit validity in domain code; replaces `NullString`, `NullInt64`, ... |
| `*T` field | The value round-trips to JSON as `null` |
| `COALESCE(col, '')` in SQL | NULL and zero mean the same to every caller |

Scanning a NULL into a plain `string`/`int64` is a runtime error, not a zero value.

### sqlc vs query builders vs ORM

| Choice | Pick when | Cost |
| --- | --- | --- |
| `database/sql` + driver | Few queries, portability, zero deps | Hand-written `Scan` lists |
| sqlc | SQL-first, many queries: typed Go generated from `.sql`, checked against the schema | Build step |
| pgx native (`pgxpool`) | Postgres-only: COPY, LISTEN/NOTIFY, arrays, faster wire protocol | Not `database/sql` |
| sqlx | Struct scanning and named args over `database/sql` | Small dep, runtime reflection |
| GORM / ent | Rarely: hidden queries, N+1 by default, implicit hooks | Debugging generated SQL |

Default to sqlc (or plain `database/sql`). Migrations: external tool (golang-migrate, Atlas, goose) as reviewed,
versioned SQL; the app never mutates its own schema at startup.

## gRPC

- `grpc.NewClient` (lazy; `Dial`/`DialContext` deprecated) once per target, sharing the `*ClientConn` (HTTP/2
  multiplexes). Headless services: `dns:///host:port` plus `round_robin`.
- Embed `pb.UnimplementedXServer` by value (not `Unsafe*`) so new RPCs compile and return `Unimplemented`. Handlers
  stay thin: decode → domain service → map errors to status.
- Never pass a downstream status through (re-map at your boundary); `Internal` messages reach the
  caller: log detail, return generic.
- Received messages cap at 4 MB (`grpc.MaxRecvMsgSize`); prefer streaming to raising it.
- Register `grpc_health_v1`; reflection off in production. `GracefulStop` in a goroutine with a timeout falling back
  to `Stop`, or a long stream blocks shutdown forever.

### Interceptors

- `grpc.ChainUnaryInterceptor(a, b, c)` / `ChainStreamInterceptor`: `a` is outermost. Order: recovery → tracing →
  logging/metrics → auth → validation. Register the stream twins too or streaming RPCs skip auth; a stream
  interceptor injecting values wraps `grpc.ServerStream` and overrides `Context()`.

### Deadlines

- Every client call carries a deadline (`context.WithTimeout`, or service-config `methodConfig.timeout`).
- The deadline travels on the wire (`grpc-timeout`); pass the handler `ctx` downstream to propagate the remaining
  budget; never `context.Background()` mid-chain.

### Status codes and details

| Code | Meaning | Client should |
| --- | --- | --- |
| `InvalidArgument` / `OutOfRange` | Bad input regardless of state | Fix the request, never retry |
| `NotFound` / `AlreadyExists` | Entity missing / duplicate on create | Act on it |
| `FailedPrecondition` | State forbids it now | Change state first, not retry |
| `Aborted` | Concurrency conflict (tx abort, version clash) | Retry the read-modify-write |
| `Unauthenticated` / `PermissionDenied` | No valid credentials / not allowed | Re-auth / give up |
| `ResourceExhausted` | Quota, rate limit, message too large | Back off |
| `Unavailable` | Transient; the only safe automatic retry | Retry with backoff |
| `DeadlineExceeded` | Budget spent; may have run | Retry only if idempotent |
| `Internal` / `Unknown` | Bug or unmapped error | Alert |

- Return `status.Error(codes.X, msg)`: a plain `error` becomes `Unknown` and breaks client retry. Map with
  `errors.Is`/`errors.As` at the handler edge.
- Details: `st, _ := status.New(codes.InvalidArgument, "bad").WithDetails(&errdetails.BadRequest{...})`, `st.Err()`;
  clients: `status.FromError(err)` → `st.Details()` and type-switch.

### Streaming lifecycle

| Side | Ends the stream by | Leak if you forget |
| --- | --- | --- |
| Server handler | Returning: `nil` → `OK`, a status error → that code | n/a |
| Client, server-stream | `Recv` until `io.EOF` or another error | Stream and goroutine stay until ctx ends |
| Client, client-stream | `CloseAndRecv` after the last `Send` | Server waits for input forever |
| Client, bidi | `CloseSend`, then drain `Recv` | Same; always `defer cancel()` the ctx |

- `Send` blocks on flow control when the peer is slow: select on `stream.Context().Done()` in producer loops.

## Picking libraries

- Routing: `ServeMux` covers methods, wildcards and precedence since 1.22; chi is the one router worth a dep
  (`http.Handler`-native, route groups); avoid fasthttp-based frameworks (fiber).
