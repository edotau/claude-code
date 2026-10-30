# Test-and-Fix Loops and Regression Tests

The rules (live first, scope, what makes a guard, when a test agent rides along) are always on in
`rules/standards/testing-guidelines.md`. This file is the optional workflow: how to run the loop, alone or as
parallel and concurrent subagent loops, and the tooling each leaf uses.

## The loop (one leaf, one scope)

1. **Scope the run.** A named path runs directly; `fast`/`unit`/`all` map to Makefile targets when defined
   (`make -n <target>`).

   | Scope | Command |
   | --- | --- |
   | One test | `go test ./internal/<pkg> -run '<TestName>' -count=1` (`-count=1` defeats the cache after an edit) |
   | Touched packages | `go test ./internal/<a> ./internal/<b>`; `-race` for concurrent code |
   | Repo tiers | `make test-smoke`/`test-unit`/`test-all` if defined, else `make test-unit`, else `make test` |

   Other languages: the overlay's scoped commands (`rules/software/<lang>/testing.md`).

2. **Triage each failure against the spec.** A failing test is guilty until its expectation traces to a spec, the
   request or observed live behavior; then the code is wrong. Name which, with the evidence. Under parallelism a red
   run is suspect — rerun solo before calling it a regression.
3. **Code wrong:** reproduce live, state the root cause ("X because Y"), fix, re-run the same command, add the
   regression test and prove its teeth (below).
4. **Re-run the scoped set.** At most 3 cycles per test (the whole task caps at 5:
   `../../problem-solving/references/verify.md`), then report it and move on. Never edit a migration or weaken an
   assertion to go green.
5. **Report:** fixed (test-wrong vs code-wrong), still failing with output, tests planned but not written.

## Loop shapes

| Situation | Shape |
| --- | --- |
| One failing test or package | Run the loop yourself, or one `copilot` leaf |
| Failures across several packages | **Parallel:** one `copilot` (or `code-worker`) leaf per failing package, in ONE message, each owning only that package's test files and running the loop above; you fan in and re-run the union scoped |
| A change is being implemented | **Concurrent:** `code-worker` ‖ `copilot` in the same message; copilot repairs the tests the change breaks now and writes new coverage after your "landed" `SendMessage` (`rules/standards/testing-guidelines.md` → Test agent) |
| A bug was fixed | **Regression leaf:** after your live before/after, one leaf writes the one regression test and returns the `test mutate` proof; several bugs in disjoint packages fan out one leaf each |
| A suite must go green, bounded | **Dynamic loop:** `Workflow green-keeper-loop` (gate → fix in scope → re-run, ≤N) — only when the user asks; one call per repo, in parallel across repos ([saved-workflows.md](saved-workflows.md)) |
| Audit findings to fix and test | `Workflow code-analyzer` (fix ‖ test per package → green-keeper-loop) — only when the user asks |

Parallel-leaf briefs carry: the package and its test files (no two leaves share a `_test.go`), the scoped command,
the 3-cycle cap, "never stash/checkout/reset/restore", and the report shape above. A leaf that hits the cap reports
and stops; it never widens its scope. Fan-in follows [the hub](../SKILL.md#fan-in): confirm each new guard can fail
before trusting it.

## Prove the teeth

```bash
claude-code test mutate <file> <old> <new> [--run RE] <pkg>   # exit 0 teeth, 1 vacuous, 2 error
```

It swaps `<old>` for `<new>` in a `go test -overlay` copy and runs the suite; the tree is never edited, so parallel
leaves on the same checkout are unaffected. Exit 2 means `<old>` was absent or ambiguous, or the mutant didn't
compile. Without the binary:

```bash
printf '{"Replace":{"%s":"%s"}}' "$PWD/<file>" /tmp/mutant.go > /tmp/ov.json   # /tmp/mutant.go = edited copy
go test -overlay /tmp/ov.json -run '<RE>' ./<pkg>    # must FAIL; the real file is untouched
```

Prove an injection landed: `assert s.count(old) == 1` before writing, grep the marker out of the file, and fail
loudly when the anchor is missing — a no-op mutation passes for the very reason you were disproving.

## Pin the behavior first — Given / When / Then

```text
Given a vault whose only agent pin is COPILOT_ACTIVE=off
When `vault list agents` renders without color
Then the copilot row reads `copilot (off)` and no PINS section is printed
```

Each criterion maps 1:1 to a test; "should work well" is not a criterion; anything untraceable is scope creep.

## Mock anti-patterns

| Anti-pattern | Gate question | Fix |
| --- | --- | --- |
| Asserting on a mock element | "Real behavior, or just mock existence?" | Unmock it, or delete the assertion |
| Test-only method on a production type | "Only used by tests?" | Move it to a `_test.go` helper; keep the seam a package var |
| Mocking without understanding | "What side effects does the real method have, and does the test need them?" | Mock only the slow/external call; keep the write path real |
| Incomplete mock | "What fields does the real API return?" | Mirror the complete real shape |

## Go shapes

| Need | Use |
| --- | --- |
| Input matrix | a table of cases + `t.Run(tc.name, …)`; one spanning test, not a row per neighbour |
| Hermetic config | `t.TempDir()` + `t.Setenv("CLAUDE_CONFIG_DIR", dir)` |
| Seam | package var swapped and restored with `t.Cleanup` (`rules/standards/testing-guidelines.md`) |
| Golden output | the package's documented `-update` flag; read the diff before accepting it |

Python (pytest, `@patch`, DRF/FastAPI contract matrices): `rules/software/python/testing.md` and
[api-contract-testing.md](api-contract-testing.md).
