# Go Services: HTTP, SQL, gRPC

Cancellation: [performance.md](performance.md#context-rules); owned test servers:
[errors-testing.md](errors-testing.md#http).

## net/http servers

- `ServeMux` GET patterns also serve HEAD; ambiguous overlaps panic during registration. Label metrics with
  `r.Pattern`, not the raw path. Prefer stdlib routing; chi when needed, avoid fasthttp frameworks.
- A zero `http.Server` has no timeouts. Set header/body/write/idle budgets; streaming needs a per-request write
  deadline via `ResponseController`, not a global timeout increase.
- Bound bodies with `MaxBytesReader`; strict JSON can reject unknown fields. Reject trailing JSON by decoding again
  and requiring EOF; `Decoder.More` checks array/object membership, not end of the document.

### Graceful shutdown

Wait for `Shutdown` itself: `ListenAndServe` returns `ErrServerClosed` before shutdown finishes. Use a fresh bounded
context. Hijacked connections are not awaited; arrange their shutdown through `RegisterOnShutdown` and track them.
On timeout fall back to `Close`. Behind a load balancer, fail readiness and allow a probe interval before stopping
HTTP, then workers and DB/gRPC.

### Middleware

Recovery → request ID/trace → metrics/logging → auth → route timeout → handler. A wrapping `ResponseWriter` exposes
`Unwrap() http.ResponseWriter` for `ResponseController`. Recovery re-panics `http.ErrAbortHandler`.

### http.Client

Reuse a client/transport per upstream; clone `DefaultTransport` and size idle connections for concurrency (default
per-host idle limit is 2). Set the overall and per-call budgets deliberately. HTTP error statuses do not set `err`;
check status, consume to EOF when practical for connection reuse, and close the response body.

## database/sql

`sql.Open` is lazy: `PingContext` checks startup connectivity. One pool per database; cap open connections across
replicas against server capacity, then set idle/lifetime limits for the proxy and load. Use context-aware calls;
close rows and check `rows.Err`. Translate driver errors in the repository so callers do not depend on the driver.

### Transactions

Use the existing transaction helper for begin, deferred rollback and commit (`ErrTxDone` after commit is expected).
Inside a transaction, a stray `db` call needs another connection and can deadlock a small pool. Keep locks short;
no network calls while holding them. Serialization/deadlock retries repeat the whole transaction, not one statement.

### NULLs

Scanning NULL into a plain string/int fails. Use `sql.Null[T]` for explicit validity, a pointer for JSON null, and
`COALESCE` only when NULL and zero mean the same thing.

### Query layer

Few queries need only `database/sql`; SQL-first code with many queries defaults to sqlc. Use pgxpool for Postgres
features or sqlx for struct scanning; avoid an ORM hiding queries unless justified. Migrations use an external tool,
not application startup.

## gRPC

- Reuse `grpc.NewClient` per target; DNS resolution plus round-robin handles headless service endpoints.
- Embed `UnimplementedXServer` by value. Map domain/downstream errors at the boundary: `Internal` text reaches clients,
  while a plain Go error becomes `Unknown`. Log details and send a safe status message.
- Prefer streaming to lifting the default 4 MB receive limit. Register health, keep production reflection off, and
  bound `GracefulStop` with fallback to `Stop`.
- Unary interceptors do not cover streams: register stream counterparts for recovery, tracing, logging, auth and
  validation. Carry deadlines downstream; retry only when the operation and status permit it.

### Streaming

A client receiving a stream drains to EOF; client-streaming ends with `CloseAndRecv`; bidirectional streams close
the send side, drain receive and cancel on exit. `Send` can block behind a slow peer: make producer loops observe
the stream context so cancellation releases them.
