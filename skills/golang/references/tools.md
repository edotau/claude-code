# Go Tools: CLIs, Modules, CI, Observability

Stdlib `flag` first, cobra when the command tree earns it, viper rarely. Modules, code lookup, CI, debugging and
runtime signals for Go 1.24+. Related: [errors-testing.md](errors-testing.md), [performance.md](performance.md).

## CLI shape: a testable `run`

- `main` builds the signal context, calls `run`, `os.Exit`s its code; nothing else calls `os.Exit` (skips defers).
- `run` takes args and streams (env/stdin when used) and returns the exit code.

```go
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
    fs := flag.NewFlagSet("tool", flag.ContinueOnError)
    fs.SetOutput(stderr)
    jsonOut := fs.Bool("json", false, "print JSON")
    if err := fs.Parse(args); err != nil {
        if errors.Is(err, flag.ErrHelp) {
            return 0
        }
        return 2 // flag already printed the message and usage to stderr
    }
    if err := work(ctx, fs.Args(), *jsonOut, stdout); err != nil {
        fmt.Fprintf(stderr, "tool: %v\n", err)
        return exitCode(err)
    }
    return 0
}
```

- Test by calling `run` with `bytes.Buffer`s; assert stdout empty on error paths, stderr empty on success. Isolate
  with `t.Setenv` and `t.TempDir()`.

### `flag` gotchas

- Always `flag.NewFlagSet(name, flag.ContinueOnError)`; global `flag.CommandLine` uses `ExitOnError` and shares state
  across tests.
- Parsing stops at the first non-flag: `tool file -v` leaves `-v` in `fs.Args()`. Document "flags first" or re-parse.
- Booleans take `-v` or `-v=false`, never `-v false` (`false` becomes a positional).
- `-flag` and `--flag` are identical; no short/long pairs or bundling. Alias: register both names on one variable.
- `fs.Visit` visits only flags that were set: tells "zero value" from "not passed".
- Custom types: `fs.Func` (one-off parser), `fs.TextVar` (`encoding.TextUnmarshaler`: `netip.Addr`, `slog.Level`),
  `flag.Value` (repeatable flags, append in `Set`).

### Subcommands

- One flag set per verb, chosen by `args[0]` from a table so help and dispatcher can't drift.
- Global flags on a root flag set parsed before the verb; avoid flags valid in both positions. Past two nesting
  levels, consider cobra.
- Requested help: stdout, exit 0. Help after a mistake: stderr, exit 2.

### Exit codes

| Code | Meaning | Typical source |
| --- | --- | --- |
| 0 | success, including requested `-h` | normal return |
| 1 | runtime failure | any error from the work |
| 2 | usage error | bad flag, wrong arg count, unknown verb |
| 3+ | tool-specific, documented outcomes | "diff found", "not logged in" (few, stable) |
| 130 / 143 | interrupted by SIGINT / SIGTERM | context cancelled by a signal |

- Map errors to codes in one place (`exitCode(err)`, `errors.As` on a typed error carrying the code); changing a code
  breaks scripts.

### stdout, stderr, stdin

- stdout is data for the next program; progress, warnings, logs, prompts and errors go to stderr.
- Color and spinners only when the stream is a terminal (`term.IsTerminal(int(f.Fd()))`) and `NO_COLOR` is unset.
- Never prompt when stdin is not a terminal; fail naming the flag that supplies the answer.
- Check the `bufio.Writer` `Flush` error; a closed pipe (`| head`) shows up there.

### Signals and cancellation

- Call `stop()` from `signal.NotifyContext` once the context is done so a second Ctrl-C kills a stuck process.
- Give shutdown its own `context.WithTimeout(context.Background(), …)`; the signal context is already cancelled.
  Goroutine lifetimes and `errgroup`: [performance.md](performance.md).
- Report an interrupt as 130/143 with one line, not a stack of "context canceled" wrappers.

### Version

- `debug.ReadBuildInfo()` gives the module version (`go install …@v1.2.3`) and `vcs.revision`/`vcs.modified`; try it
  before `-ldflags "-X main.version=…"`.
- `-X` only for values the toolchain can't know (release channel, CI date), on a package-level `var string`; it
  silently does nothing on a `const` or a misspelled path.

### cobra

Worth it for many verbs, deep nesting, generated completions, or GNU-style flags (`-n`, `--name`, `-abc`).

- `RunE`, never `Run`; `SilenceUsage` and `SilenceErrors` on the root; print the error and map the exit code in `main`.
- `root.ExecuteContext(ctx)` passes the signal context; read it via `cmd.Context()`.
- Write via `cmd.OutOrStdout()` / `cmd.ErrOrStderr()` so `SetOut`/`SetErr` capture it.
- Arg rules in `Args` (`ExactArgs`, `RangeArgs`, `MatchAll(...)`), not `len(args)` checks.
- A child's `PersistentPreRunE` replaces the parent's (no chaining); call the parent's explicitly. Hooks stop at the
  first error; `PostRunE` runs only after a successful `RunE`.
- Constraints: `MarkFlagRequired`, `MarkFlagsMutuallyExclusive`, `MarkFlagsOneRequired`, `MarkFlagsRequiredTogether`;
  `cmd.Flags().Changed(name)` = "did the user pass it".
- `StringSlice` splits on commas; use `StringArray` when values may contain commas.
- Completions: `ValidArgs` (static), `ValidArgsFunction` / `RegisterFlagCompletionFunc` (dynamic);
  `ShellCompDirectiveNoFileComp` when exhaustive. Test via the hidden `__complete` verb.
- Build the tree in `newRootCmd()`, not package-level `var`s in `init()`: flag state lives on the command and leaks
  between tests. One fresh tree per test (`SetOut`/`SetErr`/`SetArgs`).

### viper

Only when one key must resolve across flag, env and a multi-format config file in a shipped tool; otherwise one
JSON/TOML file plus `os.LookupEnv`.

- Precedence: `Set` > flag (only if `Changed`) > env > config file > remote KV > default.
- Env needs all three: `SetEnvPrefix("TOOL")`, `SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))`,
  `AutomaticEnv()`; without the replacer nested keys look for `TOOL_DB.HOST`.
- `Unmarshal` only sees known keys: `SetDefault` or `BindEnv` every key the struct reads.
- Tag fields `mapstructure:"…"`; durations and slices need decode hooks.
- A missing optional file is the value `viper.ConfigFileNotFoundError`:
  `var nf viper.ConfigFileNotFoundError; errors.As(err, &nf)` (a pointer target never matches). Only the
  name-and-path search returns it; a missing explicit `SetConfigFile` path is an `fs` error.
- `viper.New()` injected; the global leaks state across tests and goroutines.
- `WatchConfig` misses atomic-rename saves and calls back on another goroutine: decode into a fresh struct, validate,
  swap under a lock or `atomic.Pointer`.

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

### replace, workspaces, govulncheck

- `replace example.com/m => ../m` is for local development or a forked fix; it applies only in the main module, so a
  library's replace is invisible to consumers. Never ship a published library with a path replace.
- `go work init ./app ./lib` writes `go.work`, overriding replaces for every listed module. Keep it untracked in most
  repos; `GOWORK=off go build ./...` proves each module builds alone.
- govulncheck reports only vulnerabilities whose affected symbols your code can reach; `-show verbose` adds unreached
  ones. `-mode=binary ./bin/app` audits a built artifact, stdlib version included.
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
| `go.mod requires go >= 1.27 (… GOTOOLCHAIN=local)` | Install the toolchain or unset `local` |
| cgo `C compiler "gcc" not found` / link errors | `CGO_ENABLED=0`, or install gcc and `pkg-config` deps (cross-compiling needs a cross C compiler) |

```bash
go build -x -work ./cmd/app 2>&1 | less        # every compile/link command; keeps the temp dir
dlv test ./internal/store -- -test.run '^TestGet$'
dlv debug ./cmd/app -- --flag value            # builds with -gcflags=all='-N -l' for you
dlv attach "$(pgrep app)"                       # live process; pauses it, never on production
```

## Observability

### log/slog

- One logger, built in `main`, passed down or set with `slog.SetDefault` (also routes the `log` package through it).
  Libraries accept a `*slog.Logger` and never configure output.
- Constant message, variable data in attributes: `log.Info("job done", "id", id, "took", d)`.
- Two sinks: `slog.NewMultiHandler` (1.26+) before a third-party handler.
- Redact secrets at the type with `LogValue()`; never log tokens, keys or full request bodies.

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

### pprof and expvar

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
