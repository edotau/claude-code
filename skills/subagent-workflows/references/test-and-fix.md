# Test-and-Fix Loops and Regression Tests

The optional workflow behind `rules/standards/testing-guidelines.md` (live first, scope, what makes a guard, when a
test fails): how to run the loop alone or as parallel and concurrent subagent loops, and the tools each leaf uses.

## The loop (one leaf, one scope)

1. **Run the scoped set.**

   | Scope | Command |
   | --- | --- |
   | One test | `go test ./internal/<pkg> -run '<TestName>' -count=1` (`-count=1` defeats the cache after an edit) |
   | Touched packages | `go test ./internal/<a> ./internal/<b>`; `-race` for concurrent code |
   | Full suite | not a leaf's: `make test` is the full `make audit`; the main thread runs it once, with approval |

   Other languages: the overlay's commands (`rules/software/<lang>/testing.md`).
2. **Triage each failure** per the rule → When a test fails (test-wrong vs code-wrong, with evidence).
3. **Code wrong:** reproduce live, state the root cause ("X because Y"), fix, re-run the same command, add the
   regression test and prove its teeth (below).
4. **Re-run.** At most 3 cycles per test; the whole task caps at 5 (`../../problem-solving/references/verify.md`).
5. **Report:** fixed (test-wrong vs code-wrong), still failing with output, tests planned but not written.

## Loop shapes

| Situation | Shape |
| --- | --- |
| One failing test or package | The loop yourself, or one `copilot` leaf |
| Failures across several packages | **Parallel:** one `copilot` (or `code-worker`) leaf per failing package in ONE message, each owning only that package's test files; fan in and re-run the union |
| A change being implemented | **Concurrent:** `code-worker` ‖ `copilot` in one message; copilot repairs broken tests now, writes new tests after your "landed" `SendMessage` |
| A fixed bug | **Regression leaf:** after your live before/after, one leaf writes the regression test and returns the `test mutate` proof; one leaf per bug in disjoint packages |
| A suite must go green, bounded | `Workflow green-keeper-loop` (gate → fix in scope → re-run, ≤N), one call per repo — only when the user asks ([saved-workflows.md](saved-workflows.md)) |
| Audit findings to fix and test | `Workflow code-analyzer` (fix ‖ test per package → green-keeper-loop) — only when the user asks |

A parallel leaf's brief carries: its package and test files (no two leaves share a `_test.go`), the scoped command,
the 3-cycle cap, "never stash/checkout/reset/restore", and the report shape above. A leaf at the cap reports and
stops; it never widens scope. Fan in per [the hub](../SKILL.md#fan-in): confirm each new guard can fail.

## Prove the teeth

```bash
claude-code test mutate <file> <old> <new> [--run RE] <pkg>   # exit 0 teeth, 1 vacuous, 2 error
```

It swaps `<old>` for `<new>` in a `go test -overlay` copy; the tree is never edited, so parallel leaves are
unaffected. Exit 2: `<old>` absent or ambiguous, or the mutant didn't compile. Without the binary:

```bash
printf '{"Replace":{"%s":"%s"}}' "$PWD/<file>" /tmp/mutant.go > /tmp/ov.json   # /tmp/mutant.go = edited copy
go test -overlay /tmp/ov.json -run '<RE>' ./<pkg>    # must FAIL; the real file is untouched
```

Prove a mutation landed (`count(old) == 1` before writing, grep the marker after) — a no-op mutation passes for the
very reason you were disproving.

## Live first, then pin

Run the change on the real surface first, then pin what you observed (Given / When / Then where nothing runs live):
`rules/standards/testing-guidelines.md` → Live first, then tests.

## Go shapes and mock anti-patterns

| Need | Use |
| --- | --- |
| Input matrix | a case table + `t.Run(tc.name, …)` inside the one spanning test |
| Hermetic config | `t.TempDir()` + `t.Setenv("CLAUDE_CONFIG_DIR", dir)` |
| Seam | a package var swapped and restored with `t.Cleanup` (the rule's example) |
| Golden output | the package's documented `-update` flag; read the diff before accepting it |

Mocks: `rules/standards/testing-guidelines.md` → A test must guard (mock only real boundaries).

Python (pytest, `@patch`, DRF/FastAPI contract matrices): `rules/software/python/testing.md` and
[api-contract-testing.md](api-contract-testing.md).
