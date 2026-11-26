---
name: golang
description: >-
  Go gotchas and design decisions for writing, reviewing and debugging Go code: concurrency, errors, performance,
  tooling and services. Use for Go tasks; not for non-Go code.
---

# Go

Load the reference for the task, two at most. Project rules win; in the claude-code harness use
[rules/harness/go.md](../../rules/harness/go.md) for layout, CLI text, tests, lint and dependency API checks.
Verification order belongs to [quality.md](../../rules/standards/quality.md#verification-order--lint-first-always).

| Task | Reference |
| --- | --- |
| Goroutine lifetime, cancellation, contention, allocation, GC | [performance.md](references/performance.md) |
| Error boundaries, cleanup, panics, test traps, benchmarks | [errors-testing.md](references/errors-testing.md) |
| Strings, embedding, construction, refactoring | [style.md](references/style.md) |
| Flags, signals, module resolution, code lookup, observability | [tools.md](references/tools.md) |
| HTTP shutdown, SQL transactions, gRPC streams | [services.md](references/services.md) |

## Loop

Check the module's `go` directive before using a newer API. For questions local source and `go doc` cannot answer,
use the [web-explorer skill](../web-explorer/SKILL.md); its search and extraction instructions own the commands.
