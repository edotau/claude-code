# Go Tooling and Operations

Modules, code lookup, CI, debugging and runtime signals for Go 1.24+ code (module targets Go 1.27).

Harness rules win: verification order and lint-first live in
[quality.md](../../../rules/standards/quality.md); stdlib-first and pinned, audited deps in
[security.md](../../../rules/standards/security.md).

## Modules and dependencies

- Ask whether the stdlib covers it before any `go get` of a new module; a dependency is a maintenance and
  supply-chain liability you sign up for, not a convenience.
- Commit `go.sum`; `go mod verify` re-hashes the module cache against it and catches a tampered cache.
- Pin exact versions (`@v1.6.0`). MVS already picks the minimum satisfying version, so builds are reproducible
  without a lock file; floating upgrades happen only when someone runs `go get -u`.
- `go get -u ./...` lifts every direct and indirect dep to its latest minor; `-u=patch` stays within the minor.
  Prefer naming one module (`go get example.com/m@v1.7.2`) over a blanket `-u` so the diff stays reviewable.
- Remove with `go get example.com/m@none`, then `go mod tidy`. Run `tidy` before every commit that touches imports.
- Never raise the `go` line as a side effect; it gates language features and stdlib APIs for every consumer.
  `toolchain go1.27.x` only selects the toolchain for local builds; `GOTOOLCHAIN=local` refuses auto-download.

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

The `tool` directive replaces the `tools.go` blank-import file; versions live in the same `require` graph and
`go.sum`, so linters are pinned like code.

```bash
go get -tool golang.org/x/vuln/cmd/govulncheck@v1.1.4   # adds a tool line + require
go tool govulncheck ./...                                # runs the pinned build, cached
go get -u tool                                           # deliberate bump of every tool
go install tool                                          # put them all on GOBIN when needed
```

Tool deps share the module graph: a tool that requires a newer `x/sys` raises it for your code too. When that
hurts, move tools into a separate `tools/go.mod` and run `go tool -modfile=tools/go.mod <name>`.

### replace, exclude, retract, workspaces

- `replace example.com/m => ../m` is for local development or a forked fix; it applies only in the main module,
  so a library's replace is invisible to its consumers. Never ship a published library with a path replace.
- `exclude` drops one bad version from MVS; `retract` (in your own go.mod) tells consumers a tag you shipped is
  broken, and `go list -m -u` surfaces it.
- Multi-module edits: `go work init ./app ./lib` writes `go.work`, which overrides replaces for every listed
  module. Keep `go.work` untracked in most repos; `GOWORK=off go build ./...` proves each module builds alone.
- Vendoring (`go mod vendor`) gives hermetic builds; `-mod=mod` or a stale `vendor/modules.txt` produces
  "inconsistent vendoring" - rerun `go mod vendor`.

### govulncheck

- Reports only vulnerabilities whose affected symbols your code can reach, which keeps the signal high; `-show
  verbose` adds the module-level (unreached) findings.
- `govulncheck -mode=binary ./bin/app` audits a built artifact, including its embedded stdlib version.
- A stdlib finding means bump the toolchain (`toolchain` line or CI image), not a module.

## Code lookup: gopls, go doc, pkg.go.dev

Grep finds text; gopls finds meaning in the build as resolved by your `go.sum` and replaces. Pick by question:

| Question | Tool |
| --- | --- |
| Definition, references, implementers, callers in this workspace | gopls (MCP server, LSP, or CLI) |
| Signature and doc of an imported symbol, offline | `go doc pkg.Symbol`, `go doc -src`, `go doc -all pkg` |
| Browse docs locally, internal packages included (Go 1.25+) | `go doc -http` |
| Versions, licenses, importers, CVEs of a module not yet required | pkg.go.dev, `go list -m -versions` |
| Is this vuln reachable from my code | `govulncheck ./...` |

```bash
gopls definition internal/agent/run.go:42:7
gopls references -d internal/agent/run.go:42:7     # -d includes the declaration
gopls implementation internal/router/router.go:18:6
gopls call_hierarchy internal/agent/run.go:42:7
gopls rename -w internal/agent/run.go:42:7 newName  # refuses renames that break interface satisfaction
gopls check internal/agent/run.go                   # diagnostics for one file
```

- `gopls mcp` (v0.20+) exposes name- and path-based tools for agents; the CLI is debugging-grade and slow.
- Results follow the queried file's build config: references from `x_linux.go` miss `y_windows.go`. Re-run with
  `GOOS`/`-tags` set when platform files matter.
- Call hierarchy is static only; calls through func values and interface methods need `references` too.
- pkg.go.dev URLs: `pkg.go.dev/<mod>?tab=versions`, `?tab=importedby`, `?tab=licenses`. The proxy speaks plain
  HTTP (`proxy.golang.org/<mod>/@v/list`), with uppercase path letters escaped as `!` + lowercase.
- Before using an API, check the release notes (`go.dev/doc/go1.N`) against the module's `go` line; Go 1.26+
  `go fix ./...` applies the modernize analyzers to upgrade idioms in place.

## CI essentials

Run cheap and deterministic gates first so a syntax or vet failure fails fast (order:
[quality.md](../../../rules/standards/quality.md) -> Verification Order).

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

- Cache both `go env GOCACHE` and `go env GOMODCACHE`, keyed on `go.sum` and the Go version; `setup-go` does
  this when given `go-version-file: go.mod`.
- The test cache keys on inputs, env vars read and files opened; `-count=1` forces a rerun, never use it as a
  fix for a flaky test.
- `-race` needs cgo on most targets and slows tests 2-10x; run it on one OS rather than the full matrix.
- Matrix: the `go` line's version plus `stable` is enough; older versions only for a library promising them.

## Troubleshooting

Read the whole error, reproduce it as a failing test or command, change one thing at a time. Escalate tools only
when the simpler one runs out.

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
dlv test ./internal/agent -- -test.run '^TestRun$'
dlv debug ./cmd/app -- --flag value            # builds with -gcflags=all='-N -l' for you
dlv attach "$(pgrep app)"                       # live process; pauses it, never on production
```

Common compile and link errors:

- `missing go.sum entry` / `updates to go.mod needed` - run `go mod tidy`; CI should catch drift with `-diff`.
- `go.mod requires go >= 1.27 (running go 1.2x; GOTOOLCHAIN=local)` - install the toolchain or unset `local`.
- `module declares its path as X but was required as Y` - fork or rename; fix with a `replace` or the import path.
- `found packages a and b in <dir>` - two package clauses in one directory, often a stray `_test` or generated file.
- `import cycle not allowed` - move the shared type to a lower package or invert with an interface at the consumer.
- `use of internal package ... not allowed` - importer is outside the parent of `internal/`.
- `undefined: X` only on one OS or tag - the definition sits behind a `//go:build` constraint.
- cgo `C compiler "gcc" not found` / undefined symbols at link - set `CGO_ENABLED=0` or install the toolchain
  and `pkg-config` deps; cross-compiling with cgo needs a cross C compiler.

## Observability

### log/slog

- One logger, built in `main`, passed down or set with `slog.SetDefault` (which also routes the `log` package
  through it). Libraries accept a `*slog.Logger` and never configure output.
- JSON to stderr in production, text locally; level from a `slog.LevelVar` so it can change at runtime.
- Constant message, variable data in attributes: `log.Info("job done", "id", id, "took", d)`. Searchable keys
  beat interpolated strings.
- `*Context` variants (`InfoContext`) let a handler pull trace and request IDs from the context.
- Hot paths: `LogAttrs` with typed `slog.Int`/`slog.String` avoids `any` boxing; `Enabled` guards costly attrs.
- Redact at the type: implement `LogValue() slog.Value` on secret-bearing types so no call site can leak them.
- Log an error or return it, not both; the top of the call chain logs once.
- Fan-out to two sinks: `slog.NewMultiHandler` (Go 1.26+) before a third-party handler; `slog.DiscardHandler`
  for tests and silenced components.

```go
var level slog.LevelVar // zero value is Info; level.Set(slog.LevelDebug) at runtime
log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &level, AddSource: true}))
slog.SetDefault(log.With("svc", "gateway", "version", buildVersion))
```

### OpenTelemetry tracing

It is a sizable dependency tree; adopt it only when traces leave the process (a collector exists), and justify it
under [security.md](../../../rules/standards/security.md). Minimal wiring:

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

- Spans around I/O boundaries (inbound request, DB call, outbound RPC), not every function.
- `span.RecordError(err)` plus `span.SetStatus(codes.Error, ...)`; attributes stay low-cardinality.
- Context is the carrier: a call without `ctx` (`db.Query` vs `QueryContext`) silently breaks the trace.

### pprof and expvar endpoints

Both `net/http/pprof` and `expvar` register on `http.DefaultServeMux` at import. Serve that mux only on a
loopback or authenticated admin listener, never the public one:

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

- `runtime/metrics` is the stable, cheap source for GC, heap and scheduler numbers when exporting to a metrics
  system; prefer it over parsing `MemStats`.
- `trace.NewFlightRecorder` (Go 1.25+) keeps the last seconds of execution trace in memory for a post-incident
  dump without paying for always-on tracing.

Adapted from samber/cc-skills-golang (MIT): golang-dependency-management, golang-gopls, golang-pkg-go-dev,
golang-continuous-integration, golang-troubleshooting, golang-observability, golang-stay-updated, golang-how-to.
