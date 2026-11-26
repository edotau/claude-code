# Go Tools: CLIs, Modules, CI, Observability

Harness CLI, dependency APIs and lint: [go.md](../../../rules/harness/go.md). Gates:
[quality.md](../../../rules/standards/quality.md#verification-order--lint-first-always).

## CLI shape: a testable `run`

`main` owns process exit; `run` takes args and streams. `os.Exit` skips defers. Reuse the project's command table
and renderer. Cobra/viper are not used in the harness; do not introduce a parallel CLI/config stack.

### `flag` gotchas

- Use a fresh `FlagSet` with `ContinueOnError`; global flag state leaks across calls and exits on error.
- Parsing stops at the first positional; booleans use `-v=false`, not `-v false`.
  `Visit` distinguishes absent from zero.
- Each verb has its own flag set. Global flags precede the verb; avoid accepting them in two positions.

### Signals and cancellation

Call the signal context's `stop` once cancelled so a second Ctrl-C can terminate a stuck process. Shutdown needs a
fresh bounded context, not the cancelled signal context: [services.md](services.md#graceful-shutdown).

### Version

Prefer `debug.ReadBuildInfo` to injected version strings. Linker `-X` only sets a package-level string variable;
use it for metadata the toolchain cannot derive, not a constant or an unchecked symbol path.

## Modules and dependencies

- Upgrade a named module, not every dependency. Do not raise the `go` directive as a side effect; `toolchain`
  selects the preferred toolchain while `go` sets consumers' minimum. `GOTOOLCHAIN=local` disables auto-download.
- Tool directives share the application's dependency graph. Isolate tools in a separate module only when their
  requirements force unwanted application upgrades.
- A dependency's `replace` is invisible to consumers. Do not publish a library with a local-path replace.
- A workspace can hide standalone module failures: verify with `GOWORK=off`.
  Keep local `go.work` untracked in most repos.
- A reachable stdlib vulnerability needs a toolchain update, not a module bump. `govulncheck` can inspect binaries too.

## Code lookup: gopls, go doc, pkg.go.dev

Use gopls for references/rename/call hierarchy and `go doc` or module source for API contracts. gopls follows the
queried file's build configuration: repeat lookup under the relevant `GOOS`/tags to see other platform files.

## Observability

- Libraries accept the caller's logger and do not configure sinks. Use constant messages, structured attributes and
  `LogValue` for type-level redaction. Avoid formatting arguments before a disabled log call.
- Adopt OpenTelemetry when traces have a collector. `RecordError` alone does not set error status; set both, keep
  attributes low-cardinality, and flush the provider on shutdown.
- `net/http/pprof` and `expvar` register on `DefaultServeMux` at import: expose that mux only on a loopback or
  authenticated admin listener. Mutex/block profiles need sampling enabled. `trace.NewFlightRecorder` preserves
  recent trace history for incident dumps. Profile selection: [performance.md](performance.md#measure-first).
