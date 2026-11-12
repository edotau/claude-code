---
name: golang
description: Go reference for writing, reviewing and debugging Go code - concurrency and context, errors, testing and benchmarks, performance, security, style/naming/lint, design and layout, CLIs, tooling/modules/CI/observability, HTTP/SQL/gRPC services. Use for any Go task, "idiomatic Go", "review this Go", "goroutine leak", "wrap this error", "table test", "profile this", "go.mod", "golangci-lint", "slog", "graceful shutdown". Not for non-Go code.
---

# Go

Load only the reference for the task at hand; each is self-contained and general Go. A project's own rules win
where a reference disagrees.

| Task | Reference |
| --- | --- |
| Goroutines, channels, `sync`, worker pools, `context` cancellation and values | [concurrency.md](references/concurrency.md) |
| Creating, wrapping, inspecting, joining errors; panics; logging errors once | [errors.md](references/errors.md) |
| Table tests, fuzzing, golden files, `httptest`, race, benchmarks, testify | [testing.md](references/testing.md) |
| Profiling, allocations, escape analysis, maps/slices internals, PGO, GC tuning | [performance.md](references/performance.md) |
| Injection, path traversal, crypto, TLS, integer and nil safety, govulncheck | [security.md](references/security.md) |
| Naming, doc comments, modern idioms by Go version, golangci-lint | [style.md](references/style.md) |
| Interfaces, structs, options, dependency injection, package layout, refactoring | [design.md](references/design.md) |
| `flag` subcommands, exit codes, signals, cobra, viper | [cli.md](references/cli.md) |
| Modules and deps, gopls/`go doc`, CI, troubleshooting, slog/OTel/pprof | [tooling.md](references/tooling.md) |
| `net/http` servers and clients, `database/sql`, gRPC | [services.md](references/services.md) |

## Loop

1. Read the reference for the task (two at most); skip the rest.
2. Check `go.mod`'s `go` line before using a version-gated API the reference names.
3. Verify with `go build ./... && go vet ./...`, then the touched package's tests.

Sources and license: [attribution.md](attribution.md).
