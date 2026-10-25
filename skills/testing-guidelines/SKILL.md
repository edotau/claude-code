---
name: testing-guidelines
description: >-
  The harness's testing skill: run it on real data first, then write tests that actually guard —
  the import rule, proving teeth by revert-and-restore, anti-vacuity, reproducing the whole
  fault, assertions from the fixture; plus Given/When/Then, root-cause-first fixes, mocking at
  boundaries. Trigger on "write tests", "pytest", "mock", "contract test",
  "does this test have teeth", "would this catch a revert", "unguarded fix".
---
# Testing guidelines

Rules earned by shipping three tests around a real defect and then discovering none of them
would have caught it coming back. The order is always: **run it live → pin what you observed
→ prove the test bites.** Tooling (pytest, mocks, API matrices) is at the end. Dispatch policy
(who owns tests, when the test agent runs): [test discipline](references/test-discipline.md).

## Run it on data before you build the suite

**Demonstrate the feature works on real — or at least representative — input first. Write
the test suite second.** Tests written before first execution encode what you *assumed* the data
looks like; when the assumption is wrong they still pass. Test-first is right only when the
contract is already known (a specified API, a documented format, a reproduced defect); when the
contract is discovered from the data, the data is the specification.

**The tell:** the code has never consumed a real input and the test count is climbing — the only
fixtures are synthetic ones built from your beliefs about the format, or a memo/README already
states conclusions no execution produced. (A residue-scan reader once shipped with a module, a
verifier, passing tests and an interpretation memo — and had never been run on the real export.)

### The right order

1. get a real or representative input
2. run the thing; **read the output**
3. write tests that pin the behaviour you just observed
4. then add edge cases, failure injection, and the regression guard
5. only then write the document that states conclusions

### When the data is not available

Say so, and stop. The substitute for a missing input is **asking for it**, not writing more
tests. Name exactly what is needed and mark anything already written as unverified until it arrives.

### Fixtures from the live artifact

Drive parsers with real bytes sampled from the artifact they parse. Real headers range-read from
five live databases exposed two defects hand-written fixtures had hidden for months.

## The import rule

**A test that does not import the code under test is documentation, not a guard.** A test that
rebuilds the data and the arithmetic inline passes just as happily with the fix reverted.

```bash
grep -nE '^(import|from)' <test-file> | grep <module-under-test>   # no hit = no guard
```

## Prove the teeth empirically

Do not reason about whether a test would catch a regression. Demonstrate it:

1. checksum the source file
2. revert the fix **in place**
3. run the suite and confirm it fails, with a failure message matching the defect's
   signature
4. restore the file and confirm the checksum matches
5. run the suite again and confirm it passes

If the suite passes at step 3, the test is not a guard yet. The checksum is what lets you state the
tree is unchanged rather than hope so.

**Prove the injection landed.** A `python -c` one-liner with nested quotes, or a `str.replace` whose
anchor a formatter has since rewritten, silently changes nothing — and the suite passes for the very
reason you were disproving. `assert s.count(old) == 1` before writing, `grep` the injected marker out
of the file on disk, and fail loudly if the anchor is missing.

## Reproduce the whole fault, not part of it

A guard that only partially recreates the fault condition can pass over a reintroduced bug.
Ask what the worst case of the defect looks like, and build that. (Starving one prediction window
of fifteen left the total non-zero, so the test held with the fix reverted; starving every affected
window reproduced `0.0 == 0.0 : starved candidate reported as neutral` and the test bit.)

## Anti-vacuity for equivalence tests

An optimised path with a silent fallback makes "fast == reference" trivially true. So assert that the
fast path was actually taken. Observe something only it does: the input file it requests, a call
count, a cache key (`assert requested == "unique_peptides.fst"`).

Stub the shared dependency with a **deterministic oracle** — a pure function of its inputs —
so both paths receive identical values for identical inputs and any difference is genuinely
the logic under test rather than stub noise.

## Derive assertions from the fixture

Compute expected values from the fixture instead of hardcoding what you believe is in it:
`f"cand_{PARENT[19]}20W"`, not `"cand_S20W"` — otherwise a fixture surprise looks like a code bug.

## Failing loudly beats degrading quietly

When the fix itself is being designed: given a choice between continuing with incomplete
data and raising, raise. (A missing prediction logged at DEBUG produced a plausible "no impact"
answer; raising into the existing fallback turned a silent wrong result into a slower correct one.)

## Pin the behavior before the assertion

You can't assert behavior you haven't pinned down. Once the live run has shown what the code
does — or the spec states it — write the expected behavior as **Given / When / Then**:

```text
Given an authenticated user with no widgets
When they POST /api/widgets/ with {"name": "w"}
Then the response is 201 and the body contains the new widget's id
```

Each criterion maps 1:1 to a test. Vague asks get caught here ("should work well" is not a
criterion), and anything not traceable to one is scope creep you can drop.

**Stop and ask** instead of guessing when the behavior for a case isn't determinable from the
request or the data, the change alters an existing API/DB contract, or it touches
auth/permissions/PII. Don't encode a silent guess into a test that then "passes."

## Root cause before the regression test

For a reported bug the live reproduction is the RED: reproduce on the real surface, state the root
cause explicitly ("X because Y") before fixing, then re-run the same command.

## Test-and-fix loop

A named test path runs directly (`.venv/bin/python -m pytest <path> -x -v`,
`go test ./<pkg> -run '<Name>'`); `fast`/`unit`/`all` run `make test-smoke`/`test-unit`/`test-all` when
defined (`make -n <target>`), else `make test-unit`, else `make test`.

| Scope | Command |
|---|---|
| Named path | `.venv/bin/python -m pytest <path> -x -v` · `go test ./<pkg> -run '<Name>'` |
| `fast`/`unit`/`all` | `make test-smoke`/`test-unit`/`test-all` if defined, else `make test-unit`, else `make test` |

A failing test is guilty until its justification traces to a spec. Loop at most 3 cycles per test,
then report it and move on.

## Mock at boundaries, not everywhere

Mocks isolate; they are never the thing under test. Mock external IO at the consumer's import site;
more than two mocks in one test is a design smell (add a seam). Asserting on a mock's own behavior
fails the import rule in disguise.

| Anti-pattern | Gate question | Fix |
|---|---|---|
| Asserting on a mock element | "Real behavior, or just mock existence?" | Unmock it, or delete the assertion |
| Test-only method on a production class | "Only used by tests?" | Move it to test utilities |
| Mocking without understanding | "What side effects does the real method have, and does the test need them?" | Mock only the slow/external call; keep the write path real |
| Incomplete mock | "What fields does the real API return?" | Mirror the complete real shape |

## pytest quick reference

| Need | Use |
|------|-----|
| Setup/teardown, shared data | `@pytest.fixture` (scopes: function/class/module/package/session), `conftest.py` |
| Input matrix | `@pytest.mark.parametrize("a,b", [...])` |
| Isolate external calls | `@patch("consumer.module.func")`, `autospec=True` for signature safety |
| Expect an exception | `with pytest.raises(ValueError, match="...")` |
| Slow/optional tests | `@pytest.mark.slow`, `@pytest.mark.skipif(...)` |
| Async code | `@pytest.mark.asyncio` |

## API & contract testing

Unit tests prove a function behaves; contract tests prove a boundary does — request shapes, status
codes, response shape, enforced auth. Drive coverage from the auth/permission and input-validation
matrices, not the happy path: [API & contract testing](references/api-contract-testing.md).
