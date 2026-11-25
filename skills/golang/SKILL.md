---
name: golang
description: Go reference for writing, reviewing and debugging Go code - concurrency and context, errors, testing and benchmarks, performance, style/naming/lint, design and layout, CLIs, tooling/modules/CI/observability, HTTP/SQL/gRPC services. Use for any Go task, "idiomatic Go", "review this Go", "goroutine leak", "wrap this error", "table test", "profile this", "go.mod", "golangci-lint", "slog", "graceful shutdown". Not for non-Go code.
---

# Go

Load only the reference for the task at hand; each is self-contained and general Go. A project's own rules win
where a reference disagrees.

| Task | Reference |
| --- | --- |
| Goroutines, channels, `sync`, worker pools, `context`; profiling, allocations, maps/slices internals, PGO, GC | [performance.md](references/performance.md) |
| Creating, wrapping, inspecting, joining errors; panics; table tests, fuzzing, golden files, `httptest`, race, benchmarks | [errors-testing.md](references/errors-testing.md) |
| Naming, doc comments, modern idioms, golangci-lint; interfaces, structs, options, DI, package layout, refactoring | [style.md](references/style.md) |
| `flag` subcommands, exit codes, signals, cobra, viper; modules, gopls/`go doc`, CI, troubleshooting, slog/OTel/pprof | [tools.md](references/tools.md) |
| `net/http` servers and clients, `database/sql`, gRPC | [services.md](references/services.md) |

## Loop

1. Read the reference for the task (two at most); skip the rest.
2. Check `go.mod`'s `go` line before using a version-gated API the reference names.
3. Verify with `rtk go build ./... && rtk go vet ./...`, then the touched package's tests.
