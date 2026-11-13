---
paths:
  - "internal/**/*.go"
  - "cmd/claude-code/**"
---

# Go in the claude-code harness

This repo's Go conventions (module `github.com/edotau/claude-code`, `go 1.27`). They win over the general `golang`
skill; `standards/` still holds.

## Layout and placement

- `cmd/claude-code/main.go` wires and exits; all logic lives under `internal/<area>/`.
- `internal/cli` is the command table, menu and dispatcher. A verb is one `internal/cli/<verb>.go` (+ `_test.go`) or a
  table entry into the area package; the CLI file holds flags and output only, the logic lives in the area
  (table-wired: `databricks`, `doctor`, `network`, `usage`, `setup`).
- Placement by importer count: a package more than one area imports stays top-level (`internal/router`,
  `internal/tokenledger`); one area's helper nests under it (`internal/agent/tools`); a provider's code stays under
  that provider (`internal/databricks/oauth`). External SDKs are boxed one package each.
- No DI container, no cobra, no viper: wiring stays explicit in `Run`; config parsing lives in the area's package.
- Internal renames follow `rules/standards/quality.md` rule 3: no `type Old = New` alias inside the module.

## CLI and TUI text

**One line of ≤ 80 characters, label column included.** On a terminal, help and TUI menu rows never wrap: a row
wider than the window clips (`...` in help, `…` in menus). Piped help keeps the full text and may wrap only with a
hanging indent: every continuation line starts exactly at the description column. Arrow-key menu rows
(`internal/ui/menu`) stay single-line — wrapping breaks the highlight and the row→key mapping.

- Lead with what the item does; drop filler ("this will", "allows you to") and `·`-chained clauses.
- Name the one fact the user needs to choose (the key it writes, the command it runs), not every effect.
- One width test per menu table, driving the real renderer.
- `-h` text is only ever a `helptext.Page` (`helptext.NewFlagSet` + `helptext.Parse`); FLAGS rows come from the FlagSet.

```text
WRONG  Mint key      API Keys API · creates an API key in your Cloud project and prints its value
RIGHT  Mint key      create an API key in your Cloud project
RIGHT  Mint key      create an API key in your Cloud project and print its
                     value once (wrapped: continuation at the description column)
```

## Tests

### One test file per source file

- Tests for `foo.go` live in `foo_test.go` in the same directory; a test belongs to the source file that defines the
  function it mainly calls. Benchmarks, fuzz targets and examples too — ordered as their subjects appear in the source.
- Regression tests go in `foo_regression_test.go` only once there are 2 or more; a single one stays in `foo_test.go`.
- Every `*_test.go` must strip (drop `_test`, then `_regression`) to an existing `.go` in its directory. No generic
  files: `TestMain` goes in the test file of the package's main source; `export_test.go`, `helpers_test.go`,
  `<topic>_test.go` → each test, seam and helper moves to the file of the source it serves.
- A source file with 0–1 tests and no distinct role merges into the sibling it serves; its tests follow.

### Internal tests in the subject's package

- Always `package foo`, never `package foo_test` — examples included — even when the subject is elsewhere in the tree.
- An import cycle is fixed in production code (export a `SwapX`/`StubX` seam, or move what the importer needs into its
  own package), never by keeping an external test package.
- A package whose tests need registry rows from importers registers fixture rows in `TestMain` and fails the binary
  when one is missing; never rely on a sibling test file linking them in.

### Portability

Seams, mocks, speed and never spawning the test binary: `rules/standards/testing-guidelines.md` → A test must guard.

- No `//go:build` test files and no `integration` tag: skip at runtime (`if runtime.GOOS == "windows" { t.Skip(...) }`)
  and use calls that compile everywhere (`exec.Command("mkfifo", ...)`,
  `os.FindProcess(pid).Signal(syscall.Signal(0))`),
  so `GOOS=windows go vet ./...` stays green.
- A test that waits out a production timeout makes the timeout a package `var` and shrinks it in the test, with
  bounds that scale with it.

## Style and lint

- Comment cap (`rules/standards/quality.md` → Comments) applies to godoc too; a `Deprecated:` paragraph is exempt.
  A package narrative goes to `README.md`/`docs/`, never `doc.go` prose.
- `.golangci.yml` uses `linters.default: none` plus an explicit `enable` list; `modernize` is enabled. Thresholds come
  from `rules/standards/quality.md`, not upstream defaults; the repo's `gocognit` setting is its own call.
- Process-wide state (`os.Setenv`, `os.Unsetenv`, `os.Clearenv`, `os.Chdir`) goes through `internal/proc`; `forbidigo`
  fails any direct call outside it, `internal/hosttest` and tests, and per-run values travel on the run instead.
- `make lint` does not run gopls checks such as `writestring` (golang skill → `references/style.md` → Strings); the
  editor is the
  only signal for those.

## Verify and land

```bash
cd ~/.claude
go build ./... && go vet ./...
go test ./internal/<pkg>/...          # scoped while iterating
make lint                             # go, md, js, yaml; reports only (fix: make fmt)
make audit                            # full gate, once at wrap-up with the owner's approval
```

- Prove other platforms with `GOOS=linux go build ./...` / `GOOS=windows go vet ./...` on the Mac.
