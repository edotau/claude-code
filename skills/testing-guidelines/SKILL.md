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

The failure mode this prevents is subtle because it looks like diligence: a test suite grows
around code that has never processed an actual input. Every test passes. Nothing has been
learned. Tests written before first execution encode **what you assumed the data looks
like**, and when the assumption is wrong they still pass, which is worse than failing —
a red test tells you something, a green test built on a wrong premise tells you nothing
while feeling like progress.

This is not an argument against test-first. Test-first is right when the contract is
**already known**: a specified API, a documented format, a defect whose shape you have
already reproduced. It is wrong when the contract is being **discovered from the data** —
a vendor export's column names, which sign means destabilising, what an endpoint actually
returns, whether a file's numbering matches the sequence you think it does. There the data
is the specification, and you cannot write a meaningful assertion before reading it.

### The tell

Ask: *has this code ever consumed a real input?* If the answer is no and the test count is
climbing, stop and go get an input. Two specific smells:

- the only fixtures are **synthetic** ones you constructed from your own beliefs about the
  format
- a supporting document (interpretation memo, README, integration guide) already states
  conclusions that no execution has produced

### Worked example of getting it wrong

A residue-scan reader was built to parse a vendor export: the module, a parent-alignment
stage, a sign-convention verifier, a synthetic drifted fixture, passing tests, and an
interpretation memo describing what the numbers meant. It had **never been run on the real
export**, because the file was never supplied. Every number in that memo traced back to a
pasted narrative or to the fixture — that is, to assumptions.

Separately, the same effort produced three test files around a defect fix, none of which
imported the module under test (see the import rule below). Time went into test volume
while the two things that would have produced knowledge — running the reader on the real
CSV, and importing the function under test — went undone.

### The right order

1. get a real or representative input
2. run the thing; **read the output**
3. write tests that pin the behaviour you just observed
4. then add edge cases, failure injection, and the regression guard
5. only then write the document that states conclusions

### When the data is not available

Say so, and stop. The substitute for a missing input is **asking for it**, not writing more
tests. Name exactly what is needed — for the reader above: the export plus the parent
sequences, since parent alignment cannot run without them — and mark anything already
written as unverified until it arrives. A suite built over an absent input is not progress
banked; it is a claim to unpick later.

## The import rule

**A test that does not import the code under test is documentation, not a guard.**

It is easy to write a test that constructs the same data structures the function uses,
performs the same arithmetic inline, and asserts the expected result. It passes. It explains
the bug clearly. And it would pass just as happily if someone reverted the fix, because it
never touches the fixed code.

Check for this directly:

```bash
grep -nE '^(import|from)' <test-file> | grep <module-under-test>
```

No hit means no guard, whatever the test is named. Keep such tests for their explanatory
value if you like — but do not count them as protection, and do not describe the fix as
"verified" on their strength.

## Prove the teeth empirically

Do not reason about whether a test would catch a regression. Demonstrate it:

1. checksum the source file
2. revert the fix **in place**
3. run the suite and confirm it fails, with a failure message matching the defect's
   signature
4. restore the file and confirm the checksum matches
5. run the suite again and confirm it passes

Step 4 is not ceremony. Reverting a fix inside a real repository to test something and then
leaving it partly reverted is a genuinely bad outcome; the checksum is what lets you state
that the tree is unchanged rather than hope so.

If the suite passes at step 3, the test is not a guard yet.

## Reproduce the whole fault, not part of it

A guard that only partially recreates the fault condition can pass over a reintroduced bug.

Concretely: a defect zeroed the delta for a candidate when peptide predictions were missing.
The first test starved one window out of fifteen — the total stayed non-zero, so the
assertion held even with the fix reverted. Starving *every* affected window reproduced the
real signature (`0.0 == 0.0 : starved candidate reported as neutral`) and the test began to
bite.

Ask what the worst case of the defect looks like, and build that.

## Anti-vacuity for equivalence tests

When asserting that an optimised path agrees with a reference path, the optimised path often
has a **fallback**. If it silently falls back, both paths become the same code and the
equivalence assertion is trivially true — the test passes while testing nothing.

So assert that the fast path was actually taken. Observe something only it does: the input
file it requests, a call count, a cache key.

```python
# not just: assert fast_result.equals(reference_result)
# also:     assert requested == "unique_peptides.fst"   # i.e. it did NOT fall back
```

Stub the shared dependency with a **deterministic oracle** — a pure function of its inputs —
so both paths receive identical values for identical inputs and any difference is genuinely
the logic under test rather than stub noise.

## Derive assertions from the fixture

Hardcoding a value you believe is in the fixture (`assert name == "cand_S20W"`) breaks when
position 20 turns out to hold a different residue, and the failure looks like a code bug
rather than a test bug. Compute it: `f"cand_{PARENT[19]}20W"`.

## Failing loudly beats degrading quietly

When the fix itself is being designed: given a choice between continuing with incomplete
data and raising, raise. The defect above existed because a missing prediction was logged at
DEBUG and execution carried on, producing a plausible-looking "no impact" answer. Raising
into an existing fallback handler turned a silent wrong result into a slower correct one.

## Pin the behavior before the assertion

You can't assert behavior you haven't pinned down. Once the live run has shown what the code
does — or the spec states it — write the expected behavior as **Given / When / Then**:

```
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

For a reported bug the live reproduction is the RED: reproduce on the real surface, fix, re-run
the same command. Before fixing anything past a trivial bug, state the root cause explicitly
("X because Y"), trace data/control flow *backward* from the symptom, and when the failure spans
components, log at each boundary to find the layer that actually breaks. A regression test
built on the wrong root cause turns green while the real bug survives — then prove its teeth
(above).

## Mock at boundaries, not everywhere

Mocks isolate; they are never the thing under test. Mock external IO at the consumer's import
site, mirror the real object completely, and treat more than two mocks in one test as a design
smell (the code is too coupled — add a seam). Never add test-only methods to production
classes. Asserting on a mock's own behavior fails the import rule in disguise: the code under
test never ran. Full pitfalls and gate checks: `references/testing-anti-patterns.md`.

## pytest quick reference

| Need | Use |
|------|-----|
| Setup/teardown, shared data | `@pytest.fixture` (scopes: function/class/module/package/session), `conftest.py` |
| Input matrix | `@pytest.mark.parametrize("a,b", [...])` |
| Isolate external calls | `@patch("consumer.module.func")`, `autospec=True` for signature safety |
| Expect an exception | `with pytest.raises(ValueError, match="...")` |
| Slow/optional tests | `@pytest.mark.slow`, `@pytest.mark.skipif(...)` |
| Async code | `@pytest.mark.asyncio` |

Examples and `tests/` layout: `references/pytest-patterns.md`.

## API & contract testing

Unit tests prove a function behaves; **contract tests prove a boundary behaves** — accepted
request shapes, status codes, response shape, enforced auth. Drive endpoint coverage from two
matrices, not the happy path:

- **Auth/permission** — missing header (401), invalid token (401), expired (401), wrong role
  (403), not-owner (403), valid (2xx). Missing header and invalid token route differently; test both.
- **Input validation** — empty body, each required field missing, wrong type, boundaries
  (min−1/min/max/max+1), null. Bad input is 400, never 500.

Run in-process (DRF `APIClient` + `force_authenticate`, transactional rollback); reserve `httpx`
for cross-service or deployed checks. Assert the response *shape* and that sensitive fields
never leak — a 200 with the wrong body is still a bug. A data job's input/output table schema
is the same kind of contract. Route detection, full matrices, patterns:
`references/api-contract-testing.md`.

## References

- [Test discipline](references/test-discipline.md) — live checks, test-agent ownership, fixtures, gates, and injection proofs.
- `references/pytest-patterns.md` — fixtures, parametrize, mocking/patching, markers,
  exception/async tests, `tests/` layout.
- `references/api-contract-testing.md` — route detection, auth/input matrices, DRF + httpx
  patterns, data-job boundary contracts.
- `references/testing-anti-patterns.md` — testing mock behavior, test-only production methods,
  mocking without understanding, incomplete mocks.
