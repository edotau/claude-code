---
name: testing-guidelines
description: >-
  Optional test-and-fix loop: run a failing suite, fix test or code by the spec, re-run, capped at 3 cycles per test;
  plus the tooling behind the always-on testing rule — `claude-code test mutate` teeth proofs, Given/When/Then
  criteria, mock anti-patterns, pytest quick reference, API/contract matrices. Trigger on "/testing-guidelines",
  "fix failing tests", "test-and-fix loop", "does this test have teeth", "would this catch a revert", "pytest",
  "mock", "contract test".
---
# Testing guidelines — the optional loop and its tools

The rules (live first, scope, what makes a guard, the test agent) are always on in
`rules/standards/testing-guidelines.md`. This skill is the opt-in workflow and the how-to behind them.

## Test-and-fix loop

Run when asked to fix failing tests, or as the Verify step of a bug fix.

1. **Scope the run.** A named path runs directly; `fast`/`unit`/`all` map to Makefile targets when defined
   (`make -n <target>`).

   | Scope | Command |
   | --- | --- |
   | Named path | `.venv/bin/python -m pytest <path> -x -v` · `go test ./<pkg> -run '<Name>'` |
   | `fast`/`unit`/`all` | `make test-smoke`/`test-unit`/`test-all` if defined, else `make test-unit`, else `make test` |

2. **Triage each failure against the spec.** A failing test is guilty until its expectation traces to a spec,
   the request or observed live behavior; then the code is wrong. Name which, with the evidence.
3. **Bug in the code:** reproduce it live, state the root cause ("X because Y"), fix, re-run the same command, then
   add the regression test and prove it fails without the fix (below).
4. **Re-run the scoped set.** At most 3 cycles per test, then report it and move on. Never edit a migration or
   weaken an assertion to go green.
5. **Report:** fixed (test-wrong vs code-wrong), still failing with output, and tests planned but not written.

## Prove the teeth

```bash
claude-code test mutate <file> <old> <new> [--run RE] <pkg>   # exit 0 teeth, 1 vacuous, 2 error
```

It swaps `<old>` for `<new>` in a `go test -overlay` copy and runs the suite; the tree is never edited, so parallel
agents on the same checkout are unaffected. Exit 2 means `<old>` was absent or ambiguous, or the mutant didn't
compile. Without the binary:

```bash
printf '{"Replace":{"%s":"%s"}}' "$PWD/<file>" /tmp/mutant.go > /tmp/ov.json   # /tmp/mutant.go = edited copy
go test -overlay /tmp/ov.json -run '<RE>' ./<pkg>    # must FAIL; the real file is untouched
```

Prove an injection landed: `assert s.count(old) == 1` before writing, grep the marker out of the file, and fail loudly
when the anchor is missing — a no-op mutation passes for the very reason you were disproving.

## Pin the behavior first — Given / When / Then

```text
Given an authenticated user with no widgets
When they POST /api/widgets/ with {"name": "w"}
Then the response is 201 and the body contains the new widget's id
```

Each criterion maps 1:1 to a test; "should work well" is not a criterion; anything untraceable is scope creep.

## Mock anti-patterns

| Anti-pattern | Gate question | Fix |
| --- | --- | --- |
| Asserting on a mock element | "Real behavior, or just mock existence?" | Unmock it, or delete the assertion |
| Test-only method on a production class | "Only used by tests?" | Move it to test utilities |
| Mocking without understanding | "What side effects does the real method have, and does the test need them?" | Mock only the slow/external call; keep the write path real |
| Incomplete mock | "What fields does the real API return?" | Mirror the complete real shape |

## pytest quick reference

| Need | Use |
| --- | --- |
| Setup/teardown, shared data | `@pytest.fixture` (function/class/module/package/session), `conftest.py` |
| Input matrix | `@pytest.mark.parametrize("a,b", [...])` |
| Isolate external calls | `@patch("consumer.module.func")`, `autospec=True` for signature safety |
| Expect an exception | `with pytest.raises(ValueError, match="...")` |
| Slow/optional tests | `@pytest.mark.slow`, `@pytest.mark.skipif(...)` |
| Async code | `@pytest.mark.asyncio` |

## API & contract testing

Contract tests prove a boundary — request shapes, status codes, response shape, enforced auth. Drive coverage from
the auth/permission and input-validation matrices, not the happy path:
[API & contract testing](references/api-contract-testing.md).
