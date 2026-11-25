# Go Services: HTTP, SQL, gRPC

Related: [concurrency.md](concurrency.md), [errors.md](errors.md), [testing.md](testing.md) (`httptest`).

## net/http servers

- Go 1.22 `ServeMux`: `"GET /items/{id}"` + `r.PathValue("id")`, `{rest...}` tail, `{$}` exact `/`. `GET` serves
  `HEAD`; wrong method → automatic 405 + `Allow`. Ambiguous overlaps panic at `Handle`: register in `main`.
- Label metrics/logs with `r.Pattern` (1.23), never `r.URL.Path`.
- Never `http.ListenAndServe` or a zero `http.Server`: every timeout is off.

| Field | Bounds | Typical |
| --- | --- | --- |
| `ReadHeaderTimeout` | Headers (Slowloris; gosec G112) | 5s |
| `ReadTimeout` | Whole request incl. body | 15–30s |
| `WriteTimeout` | Header read → response written; caps handler time | 30s; 0 for streaming |
| `IdleTimeout` | Keep-alive gap | 60–120s |

- Long/streaming handlers: keep server timeouts tight, lift per request with
  `http.NewResponseController(w).SetWriteDeadline(...)`.
- Cap bodies: `http.MaxBytesReader(w, r.Body, n)`; `json.Decoder` with `DisallowUnknownFields` where strict, reject
  trailing data (`dec.More()`).

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
    return err // bind failure
case <-ctx.Done():
}
shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()
return srv.Shutdown(shutCtx) // wait here: ListenAndServe returns ErrServerClosed at once
```

- `Shutdown` skips hijacked conns (websockets): close them via `RegisterOnShutdown`; on timeout call `srv.Close()`.
- Behind a load balancer: fail readiness, wait one probe interval, then shut down HTTP → workers → DB/gRPC.

### Middleware

- Outermost first: recover → request ID/trace → logging/metrics → auth → per-route timeout → handler.
- A wrapping `ResponseWriter` needs `Unwrap() http.ResponseWriter`, or `ResponseController` silently breaks.
- Own panic recovery so you return a 500 with context; re-panic `http.ErrAbortHandler`.

### http.Client

- One shared client per upstream; a client or `Transport` per request defeats pooling and leaks sockets.
- `http.DefaultClient` has no timeout: set `Client.Timeout` and use `NewRequestWithContext`.
- Clone `http.DefaultTransport`; raise `MaxIdleConnsPerHost` (default 2) for a hot upstream.
- 4xx/5xx is `err == nil`: check `resp.StatusCode`.

## database/sql

- `sql.Open` doesn't connect: `PingContext` at startup. One `*sql.DB` per database, for life.

| Setter | Default | Rule |
| --- | --- | --- |
| `SetMaxOpenConns` | unlimited | Always set; replicas × value ≤ server `max_connections` |
| `SetMaxIdleConns` | 2 | ≈ MaxOpen under steady load |
| `SetConnMaxLifetime` | forever | Below any LB/proxy idle cut-off |
| `SetConnMaxIdleTime` | forever | 1–5 min to shed bursts |

- Use only `*Context` methods; the plain forms outlive the caller and hold a connection.
- Placeholders for values; dynamic identifiers only from an allowlist.
- `defer rows.Close()` after the error check; return `rows.Err()` after the loop.
- Map `sql.ErrNoRows` with `errors.Is` to a domain `ErrNotFound`; translate driver errors (`*pgconn.PgError`) with
  `errors.As` in the repository so callers never import the driver.

### Transactions

- One `withTx(ctx, db, opts, fn)`: `BeginTx`, `defer tx.Rollback()` (ignores `ErrTxDone`), `fn(tx)`, `Commit`.
- Inside `fn` use only `tx`: a stray `db.` call takes a second connection and can deadlock a small pool.
- Read-modify-write: `SELECT ... FOR UPDATE` (`SKIP LOCKED` for queues), or serializable + retry the whole `withTx`
  on `40001`/`40P01`.
- A tx is one connection; keep it short, no network calls while holding locks.

### NULLs

| Option | Use when |
| --- | --- |
| `sql.Null[T]` (1.22) | Explicit validity in domain code |
| `*T` | Must round-trip to JSON `null` |
| `COALESCE(col, '')` | NULL and zero mean the same |

Scanning NULL into a plain `string`/`int64` is a runtime error.

### Query layer

| Choice | Pick when |
| --- | --- |
| `database/sql` + driver | Few queries, zero deps |
| sqlc (default) | SQL-first, many queries; typed Go checked against the schema |
| `pgxpool` | Postgres-only features (COPY, LISTEN/NOTIFY, arrays) |
| sqlx | Struct scanning over `database/sql` |
| GORM / ent | Rarely: hidden queries, N+1 by default |

Migrations: an external tool (golang-migrate, Atlas, goose); never self-migrate at startup.

## gRPC

- `grpc.NewClient` once per target, share the `*ClientConn`; headless services: `dns:///host:port` + `round_robin`.
- Embed `pb.UnimplementedXServer` by value; handlers decode → domain → map errors to status.
- Re-map downstream statuses at your boundary; `Internal` messages reach callers: log detail, return generic.
- 4 MB receive cap (`MaxRecvMsgSize`): stream instead of raising it.
- Register `grpc_health_v1`; reflection off in production; `GracefulStop` with a timeout falling back to `Stop`.
- Interceptors: `ChainUnaryInterceptor(a, b, c)`, `a` outermost: recovery → tracing → logging → auth → validation.
  Register the stream twins too, or streams skip auth.
- Every call carries a deadline; pass the handler `ctx` downstream, never `context.Background()` mid-chain.

### Status codes

| Code | Client should |
| --- | --- |
| `InvalidArgument` / `OutOfRange` | Fix the request; never retry |
| `NotFound` / `AlreadyExists` | Act on it |
| `FailedPrecondition` | Change state first |
| `Aborted` | Retry the read-modify-write |
| `Unauthenticated` / `PermissionDenied` | Re-auth / give up |
| `ResourceExhausted` | Back off |
| `Unavailable` | Retry with backoff (the one safe auto-retry) |
| `DeadlineExceeded` | Retry only if idempotent |
| `Internal` / `Unknown` | Alert |

- Return `status.Error(codes.X, msg)`: a plain `error` becomes `Unknown`. Details via `WithDetails`; clients read
  `status.FromError(err).Details()`.

### Streaming

| Side | Ends the stream by | Forgetting leaks |
| --- | --- | --- |
| Server | Returning (`nil` → `OK`) | n/a |
| Client, server-stream | `Recv` until `io.EOF` | Stream until ctx ends |
| Client, client-stream | `CloseAndRecv` | Server waits forever |
| Client, bidi | `CloseSend`, drain `Recv`, `defer cancel()` | Same |

- `Send` blocks on a slow peer: select on `stream.Context().Done()` in producer loops.

## Libraries

- `ServeMux` covers routing since 1.22; chi is the one router worth a dep; avoid fasthttp frameworks (fiber).
