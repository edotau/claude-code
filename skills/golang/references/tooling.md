# Go Tooling and Operations

Modules, code lookup, CI, debugging and runtime signals for Go 1.24+ code.

## Modules and dependencies

- `go get -u ./...` lifts every dep to its latest minor; `-u=patch` stays within the minor. Prefer naming one module
  (`go get example.com/m@v1.7.2`) so the diff stays reviewable.
- Never raise the `go` line as a side effect: it gates language features and stdlib APIs for every consumer.
  `toolchain go1.27.x` only selects the local toolchain; `GOTOOLCHAIN=local` refuses auto-download.

| Command | Answers |
| --- | --- |
| `go mod why -m example.com/m` | Shortest import chain that pulls the module in |
| `go mod graph \| grep ' example.com/m@'` | Who requires which version (edges, not the selected set) |
| `go list -m all` | The selected build list after MVS |
| `go list -m -u all` | Which selected modules have newer versions |
| `go list -m -versions example.com/m` | Every tagged version the proxy knows |
| `go version -m ./bin/app` | Module versions and build settings embedded in a binary |
| `go mod edit -json` / `-go=1.27` / `-droprequire` | Scripted, non-interactive go.mod edits |

### Tools in go.mod (Go 1.24+)

The `tool` directive replaces the `tools.go` blank-import file; versions share the `require` graph and `go.sum`.

```bash
go get -tool golang.org/x/vuln/cmd/govulncheck@v1.1.4   # adds a tool line + require
go tool govulncheck ./...                                # runs the pinned build, cached
go get -u tool                                           # deliberate bump of every tool
go install tool                                          # put them all on GOBIN when needed
```

A tool requiring a newer `x/sys` raises it for your code too; if that hurts, use a separate `tools/go.mod` and
`go tool -modfile=tools/go.mod <name>`.

### replace, exclude, retract, workspaces

- `replace example.com/m => ../m` is for local development or a forked fix; it applies only in the main module, so a
  library's replace is invisible to consumers. Never ship a published library with a path replace.
- Multi-module edits: `go work init ./app ./lib` writes `go.work`, overriding replaces for every listed module. Keep
  it untracked in most repos; `GOWORK=off go build ./...` proves each module builds alone.

### govulncheck

- Reports only vulnerabilities whose affected symbols your code can reach; `-show verbose` adds unreached ones.
- `govulncheck -mode=binary ./bin/app` audits a built artifact, including its stdlib version.
- A stdlib finding means bump the toolchain (`toolchain` line or CI image), not a module.

## Code lookup: gopls, go doc, pkg.go.dev

Grep finds text; gopls finds meaning in the build as resolved by your `go.sum` and replaces.

| Question | Tool |
| --- | --- |
| Definition, references, implementers, callers in this workspace | gopls (MCP server, LSP, or CLI) |
| Signature and doc of an imported symbol, offline | `go doc pkg.Symbol`, `go doc -src`, `go doc -all pkg` |
| Browse docs locally, internal packages included (Go 1.25+) | `go doc -http` |
| Versions, licenses, importers, CVEs of a module not yet required | pkg.go.dev, `go list -m -versions` |
| Is this vuln reachable from my code | `govulncheck ./...` |

```bash
gopls definition internal/store/store.go:42:7
gopls references -d internal/store/store.go:42:7     # -d includes the declaration
gopls implementation internal/store/cache.go:18:6
gopls call_hierarchy internal/store/store.go:42:7
gopls rename -w internal/store/store.go:42:7 newName  # refuses renames that break interface satisfaction
gopls check internal/store/store.go                   # diagnostics for one file
```

- `gopls mcp` (v0.20+) exposes name- and path-based tools for agents; the CLI is debugging-grade and slow.
- Results follow the queried file's build config: references from `x_linux.go` miss `y_windows.go`; re-run with
  `GOOS`/`-tags` when platform files matter.

## CI essentials

Cheap, deterministic gates first so a syntax or vet failure fails fast.

| Step | Command | Why |
| --- | --- | --- |
| Module hygiene | `go mod tidy -diff` (Go 1.23+) | Fails when go.mod/go.sum drift, without editing |
| Format | `test -z "$(gofmt -l .)"` | Unformatted files fail the build |
| Vet | `go vet ./...` | Printf, copylocks, lostcancel, unreachable, struct tags |
| Cross-OS vet | `GOOS=windows go vet ./...` (plus `darwin`) | Compiles platform-tagged files the host skips |
| Lint | `go tool golangci-lint run` | Pinned via `tool`, same version locally and in CI |
| Test | `go test -race -shuffle=on ./...` | Races and order-dependent tests |
| Vulns | `go tool govulncheck ./...` | Reachable CVEs in deps and stdlib |
| Build | `CGO_ENABLED=0 go build -trimpath ./...` | Reproducible, path-free binaries |

## Troubleshooting

| Symptom | First move |
| --- | --- |
| Build fails oddly or depends on env | `go env` (GOFLAGS, GOOS, CGO_ENABLED, GOWORK), then `go build -x -v` |
| Wrong file compiled or missing | `go list -f '{{.GoFiles}} {{.IgnoredGoFiles}}' ./pkg` |
| Crash with partial stack | `GOTRACEBACK=all` (every goroutine), `=crash` for a core dump |
| Hang | SIGQUIT (`kill -QUIT`) dumps all goroutines; or `/debug/pprof/goroutine?debug=2` |
| Flaky test | `go test -race -count=50 -run '^TestX$' ./pkg`, then `-shuffle=<seed>` from the failure |
| GC or scheduler suspicion | `GODEBUG=gctrace=1` or `schedtrace=1000` |
| Regression with a known good commit | `git bisect run go test -run '^TestX$' ./pkg` |

```bash
go build -x -work ./cmd/app 2>&1 | less        # every compile/link command; keeps the temp dir
dlv test ./internal/store -- -test.run '^TestGet$'
dlv debug ./cmd/app -- --flag value            # builds with -gcflags=all='-N -l' for you
dlv attach "$(pgrep app)"                       # live process; pauses it, never on production
```

Common compile and link errors:

- `go.mod requires go >= 1.27 (running go 1.2x; GOTOOLCHAIN=local)` - install the toolchain or unset `local`.
- cgo `C compiler "gcc" not found` / undefined symbols at link - `CGO_ENABLED=0` or install the toolchain and
  `pkg-config` deps; cross-compiling with cgo needs a cross C compiler.

## Observability

### log/slog

- One logger, built in `main`, passed down or set with `slog.SetDefault` (also routes the `log` package through it).
  Libraries accept a `*slog.Logger` and never configure output.
- Constant message, variable data in attributes: `log.Info("job done", "id", id, "took", d)`.
- Two sinks: `slog.NewMultiHandler` (1.26+) before a third-party handler.
- Redact at the type (`LogValue()`): [security.md](security.md) → Secrets in process.

```go
var level slog.LevelVar // zero value is Info; level.Set(slog.LevelDebug) at runtime
log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &level, AddSource: true}))
slog.SetDefault(log.With("svc", "gateway", "version", buildVersion))
```

### OpenTelemetry tracing

A sizable dependency tree; adopt only when traces leave the process (a collector exists). Minimal wiring:

```go
exp, err := otlptracehttp.New(ctx) // endpoint from OTEL_EXPORTER_OTLP_ENDPOINT
if err != nil {
    return fmt.Errorf("otlp exporter: %w", err)
}
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
defer tp.Shutdown(context.Background()) // flushes buffered spans
otel.SetTracerProvider(tp)
otel.SetTextMapPropagator(propagation.TraceContext{})
handler = otelhttp.NewHandler(mux, "http.server")
```

- `span.RecordError(err)` plus `span.SetStatus(codes.Error, ...)`; keep attributes low-cardinality.

### pprof and expvar endpoints

`net/http/pprof` and `expvar` register on `http.DefaultServeMux` at import. Serve that mux only on a loopback or
authenticated admin listener, never the public one:

```go
admin := http.NewServeMux()
admin.HandleFunc("/debug/pprof/", pprof.Index)
admin.HandleFunc("/debug/pprof/profile", pprof.Profile)
admin.HandleFunc("/debug/pprof/trace", pprof.Trace)
admin.Handle("/debug/vars", expvar.Handler())
go func() { errc <- http.ListenAndServe("127.0.0.1:6060", admin) }()
```

| Endpoint | Use |
| --- | --- |
| `go tool pprof -http=: host:6060/debug/pprof/profile?seconds=30` | CPU hotspots |
| `.../debug/pprof/heap` (`-sample_index=alloc_space` for churn) | Live heap or allocation volume |
| `.../debug/pprof/goroutine?debug=2` | Full stacks: leaks and deadlocks |
| `.../debug/pprof/mutex`, `.../block` | Contention; off until `runtime.SetMutexProfileFraction`/`SetBlockProfileRate` |
| `curl host:6060/debug/vars` | `expvar` counters plus `memstats` and `cmdline` as JSON |

- `trace.NewFlightRecorder` (Go 1.25+) keeps the last seconds of execution trace in memory for a post-incident dump.
