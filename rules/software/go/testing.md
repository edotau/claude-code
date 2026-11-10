---
paths:
  - "**/*.go"
---
# Go Testing

> This file extends [standards/testing-guidelines.md](../../standards/testing-guidelines.md)

## One test file per source file

- Tests for `foo.go` live in `foo_test.go`, in the same directory. A test belongs to the source file that defines the
  function it mainly calls.
- Regression tests for `foo.go` live in `foo_regression_test.go`, but only once there are 2 or more; a single
  regression test goes in `foo_test.go`.
- Every `*_test.go` must strip (drop `_test`, then `_regression`) to an existing `.go` in its directory. No generic
  files: `main_test.go` → `TestMain` goes in the test file of the package's main source; `export_test.go`,
  `helpers_test.go`, `fixes_test.go`, `<topic>_test.go` → each test, seam and helper moves to the file of the source
  it serves.
- A source file with 0–1 tests and no distinct role merges into the sibling it serves; its tests follow.

## Internal tests in the subject's package

- A test lives in the package that defines its subject, as an internal test (`package foo`, never `package foo_test`),
  even when that package is elsewhere in the tree.
- An import cycle is fixed in production code — export a `SwapX`/`StubX` seam, or move the code the importer needs into
  its own package (`internal/agent/launchcli`) — never by keeping an external test package.
- A package whose tests need registry rows from importers registers fixture rows in `TestMain` and fails the binary
  when one is missing; never rely on a sibling test file linking them in.

## Portable test files

- No `//go:build` test files: skip at runtime (`if runtime.GOOS == "windows" { t.Skip(...) }`) and use calls that
  compile everywhere (`exec.Command("mkfifo", ...)`, `os.FindProcess(pid).Signal(syscall.Signal(0))`), so
  `GOOS=windows go vet ./...` stays green.
- A test that waits out a production timeout makes the timeout a package `var` and shrinks it in the test, with bounds
  that scale with it.
