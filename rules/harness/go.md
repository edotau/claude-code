---
paths:
  - "internal/**/*.go"
  - "cmd/claude-code/**"
---

# Go in the claude-code harness

This repo's Go conventions (module `github.com/edotau/claude-code`). They win over the general `golang` skill;
`standards/` still holds.

## Layout and placement

- `cmd/claude-code/main.go` wires and exits; all logic lives under `internal/<area>/`.
- `internal/cli` is the command table, menu and dispatcher. A verb is a `cmd<Verb>` function in `internal/cli/`
  (usually in `<verb>.go` with `<verb>_test.go`; small verbs share their area's file) or a table entry into the area
  package; the CLI file holds flags and output only (table-wired: `databricks`, `doctor`, `network`, `usage`,
  `setup`).
- Placement by importer count: a package more than one area imports stays top-level (`internal/router`,
  `internal/tokenledger`); one area's helper nests under it (`internal/agent/pack`); a provider's code stays under
  that provider (`internal/databricks/oauth`). External SDKs are boxed one package each.
- No DI container, cobra or viper: wiring stays explicit in `Run`; config parsing lives in the area's package.

## CLI and TUI text

**One line of ≤ 80 characters, label column included.** On a terminal, help and TUI menu rows never wrap: a wider row
clips (`...` in help, `…` in menus). Piped help keeps the full text and may wrap only with a hanging indent starting at
the description column. Arrow-key menu rows (`internal/ui/menu`) stay single-line.

- Lead with what the item does; drop filler ("this will", "allows you to") and `·`-chained clauses; name the one fact
  the user needs to choose (the key it writes, the command it runs).
- One width test per menu table, driving the real renderer.
- `-h` text is only a `helptext.Page` (`helptext.NewFlagSet` + `helptext.Parse`); FLAGS rows come from the FlagSet.

## Tests

- **One test file per source file:** tests for `foo.go` live in `foo_test.go` beside it (benchmarks, fuzz, examples
  too), ordered as their subjects appear. Regression tests go in `foo_regression_test.go` only once there are 2 or
  more. Layout (every `*_test.go` strips to an existing `.go`, internal `package foo` only, no build tags) is enforced
  by `claude-code review simplify --fail`; the one exception is `internal/memory/jobscope_test.go` (import cycle).
  `TestMain` goes in the main source's test file. A source file with 0–1 tests and no distinct role merges into the
  sibling it serves.
- **Internal tests:** fix an import cycle in production code (export a `SwapX`/`StubX` seam, or move what the importer
  needs into its own package). A package whose tests need registry rows from importers registers fixture rows in
  `TestMain` and fails when one is missing.
- **Portability:** seams, mocks and speed are in `rules/standards/testing-guidelines.md` → A test must guard. Skip
  at runtime (`runtime.GOOS == "windows"` → `t.Skip`) and use
  calls that compile everywhere, so `GOOS=windows go vet ./...` stays green. A test that waits out a production
  timeout makes it a package `var` and shrinks it, with bounds that scale with it.

## Style and lint

- The comment cap (`rules/standards/quality.md` → Comments) applies to godoc too; a `Deprecated:` paragraph is exempt;
  a package narrative goes to `README.md`/`docs/`, never `doc.go` prose.
- `.golangci.yml` uses `linters.default: none` plus an explicit `enable` list (`modernize` included); its
  one threshold is `gocognit` `min-complexity: 30`, and the `quality.md` size, import and nesting caps are enforced by
  `claude-code review simplify --fail`, not golangci.
- Process-wide state (`os.Setenv`, `os.Unsetenv`, `os.Clearenv`, `os.Chdir`) goes through `internal/proc`; `forbidigo`
  fails a direct call outside it, `internal/hosttest` and tests; per-run values travel on the context
  (`proc.WithValues`, `agent.RunValues`).
- `make lint` and `agents diff --check` do not run gopls checks such as `writestring` (golang skill →
  `references/style.md` → Strings); only the editor shows those.
- Verify per `quality.md` → Verification Order (`go test ./internal/<pkg>/...` scoped,
  `agents diff --check --path <files>`, `make audit` at wrap-up); prove other platforms with `GOOS=linux go build ./...`
  / `GOOS=windows go vet ./...`.
