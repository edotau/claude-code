---
name: testing-guidelines
description: >-
  The harness's testing skill: run it on real data first, then write tests that actually guard —
  the import rule, proving teeth by revert-and-restore, anti-vacuity for equivalence tests,
  reproducing the whole fault, assertions derived from the fixture; plus Given/When/Then
  criteria, root-cause-first bug fixes, mocking at boundaries, pytest fixtures/parametrize/
  mocking, and API/contract tests. Trigger on "write tests", "pytest", "mock", "API tests",
  "contract test", "test this endpoint", "integration test", "does this test have teeth",
  "would this catch a revert", "unguarded fix", or reviewing a test that accompanies a bug fix.
---
# Testing guidelines

Rules earned by shipping three tests around a real defect and then discovering none of them
would have caught it coming back. The order is always: **run it live → pin what you observed
→ prove the test bites.** Tooling (pytest, mocks, API matrices) is at the end.

## Run it on data before you build the suite

**Demonstrate the feature works on real — or at least representative — input first. Write
the test suite second.**

[Live-first rationale and examples](references/live-first.md).

### The right order

1. get a real or representative input
2. run the thing; **read the output**
3. write tests that pin the behaviour you just observed
4. then add edge cases, failure injection, and the regression guard
5. only then write the document that states conclusions

### When the data is not available

Say so, and stop. The substitute for a missing input is **asking for it**, not writing more
tests.

[Missing-input example](references/live-first.md#when-the-data-is-not-available).

## The import rule

**A test that does not import the code under test is documentation, not a guard.**

[Import check](references/guard-examples.md#the-import-rule).

## Prove the teeth empirically

Do not reason about whether a test would catch a regression. Demonstrate it:

1. checksum the source file
2. revert the fix **in place**
3. run the suite and confirm it fails, with a failure message matching the defect's
   signature
4. restore the file and confirm the checksum matches
5. run the suite again and confirm it passes

If the suite passes at step 3, the test is not a guard yet.

[Checksum rationale](references/guard-examples.md#prove-the-teeth-empirically).

## Reproduce the whole fault, not part of it

A guard that only partially recreates the fault condition can pass over a reintroduced bug.

Ask what the worst case of the defect looks like, and build that.

[Worked regression](references/guard-examples.md#reproduce-the-whole-fault-not-part-of-it).

## Anti-vacuity for equivalence tests

So assert that the fast path was actually taken. Observe something only it does: the input
file it requests, a call count, a cache key.

Stub the shared dependency with a **deterministic oracle** — a pure function of its inputs —
so both paths receive identical values for identical inputs and any difference is genuinely
the logic under test rather than stub noise.

[Anti-vacuity example](references/guard-examples.md#anti-vacuity-for-equivalence-tests).

## Derive assertions from the fixture

[Fixture-derived assertion example](references/guard-examples.md#derive-assertions-from-the-fixture).

## Failing loudly beats degrading quietly

When the fix itself is being designed: given a choice between continuing with incomplete
data and raising, raise.

[Silent-degradation example](references/guard-examples.md#failing-loudly-beats-degrading-quietly).

## Pin the behavior before the assertion

You can't assert behavior you haven't pinned down. Once the live run has shown what the code
does — or the spec states it — write the expected behavior as **Given / When / Then**:

Each criterion maps 1:1 to a test. Vague asks get caught here ("should work well" is not a
criterion), and anything not traceable to one is scope creep you can drop.

**Stop and ask** instead of guessing when the behavior for a case isn't determinable from the
request or the data, the change alters an existing API/DB contract, or it touches
auth/permissions/PII. Don't encode a silent guess into a test that then "passes."

[Worked criterion](references/guard-examples.md#pin-the-behavior-before-the-assertion).

## Root cause before the regression test

For a reported bug the live reproduction is the RED: reproduce on the real surface, fix, re-run
the same command. Before fixing anything past a trivial bug, state the root cause explicitly
("X because Y"), trace data/control flow *backward* from the symptom, and when the failure spans
components, log at each boundary to find the layer that actually breaks. A regression test
built on the wrong root cause turns green while the real bug survives — then prove its teeth
(above).

## Mock at boundaries, not everywhere

Mocks isolate; they are never the thing under test.

[Mocking rules and pitfalls](references/testing-anti-patterns.md#mock-at-boundaries-not-everywhere).

## pytest quick reference

[pytest quick reference](references/pytest-patterns.md#quick-reference), examples, and `tests/` layout.

## API & contract testing

[API & contract testing](references/api-contract-testing.md#boundary-checklist) — boundary rules, route detection, matrices, and patterns.

## References

- [Test discipline](references/test-discipline.md) — live checks, test-agent ownership, fixtures, gates, and injection proofs.
- `references/pytest-patterns.md` — fixtures, parametrize, mocking/patching, markers,
  exception/async tests, `tests/` layout.
- `references/api-contract-testing.md` — route detection, auth/input matrices, DRF + httpx
  patterns, data-job boundary contracts.
- `references/testing-anti-patterns.md` — testing mock behavior, test-only production methods,
  mocking without understanding, incomplete mocks.
