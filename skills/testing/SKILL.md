---
name: testing
description: 'TDD and Python testing: Red-Green-Refactor, acceptance criteria (Given/When/Then) before the test, pytest fixtures/parametrize/mocking, and API/contract tests for DRF endpoints. Triggers "TDD", "write tests first", "pytest", "mock", "acceptance criteria", "API tests", "contract test", "test this endpoint", "integration test".'

metadata:
  version: 1.0.0
---

# Testing

Test-driven development workflow and comprehensive Python testing patterns.

## Part 1: Test-Driven Development

### Iron Law

**NO PRODUCTION CODE WITHOUT A FAILING TEST FIRST.**

This is not a suggestion. This is the workflow:

```
RED    → Write test that describes desired behavior. Run it. It MUST fail.
GREEN  → Write minimal code to make the test pass. Nothing more.
REFACTOR → Clean up while keeping tests green. Improve design.
```

### Common Rationalizations (All Wrong)

| Rationalization | Why It's Wrong |
|----------------|---------------|
| "This is too simple to test" | Simple code becomes complex. Test it now. |
| "I'll write tests after" | You won't. And the code won't be testable. |
| "It's just a refactor" | Refactors break things. Tests catch it. |
| "The types guarantee correctness" | Types don't test behavior. |
| "It's just a config change" | Config changes cause production incidents. |
| "I need to prototype first" | Prototypes become production. Test from the start. |
| "Tests slow me down" | Debugging without tests is what slows you down. |
| "It's a private function" | Test through the public interface. |
| "The integration test covers it" | Integration tests are slow and don't isolate. |
| "I'll mock everything" | Over-mocking tests nothing. Mock at boundaries. |
| "100% coverage is the goal" | Meaningful coverage is the goal. Don't test getters. |

### Red Flags

Stop and re-evaluate if you:
- Write production code before a failing test
- Have a test that passes on first run (did you really write it first?)
- Mock more than 2 dependencies in a single test
- Can't describe what behavior the test verifies

### Debugging Integration

When a bug is reported:
1. Write a test that reproduces the bug (RED)
2. Fix the bug (GREEN)
3. The test prevents regression forever

**Confirm the root cause before you fix it.** For anything past a trivial bug, don't act on the
first plausible cause — "obvious" root causes are wrong a surprising fraction of the time. State
it explicitly ("I think X is the root cause because Y"), trace the data/control flow *backward*
from the symptom rather than trusting the surface, and if the failure spans components, add
diagnostic logging at each boundary to find the layer that actually breaks. Only then write the
reproducing test. A test built on the wrong root cause turns green while the real bug survives.

### When Stuck

| Problem | Solution |
|---------|----------|
| Don't know how to test | Write the wished-for API. Write the assertion first. |
| Test too complicated | Design too complicated. Simplify the interface. |
| Must mock everything | Code too coupled. Use dependency injection. |
| Test setup huge | Extract helpers. Still complex? Simplify the design. |

If the implementation already exists before its test, that's not TDD — delete it and start fresh from the test. "Keep it as reference" means you'll adapt it, which is testing-after.

For deeper mock/test-utility pitfalls (testing mock behavior instead of real behavior, test-only methods on production classes), see `references/testing-anti-patterns.md`.

### Acceptance criteria come before the test

RED begins one step earlier than the first `assert`: you can't write a test for behavior you
haven't pinned down. Before touching the test file, state the behavior as
**Given / When / Then** criteria — the concrete answer to "what does done look like?":

```
Given an authenticated user with no widgets
When they POST /api/widgets/ with {"name": "w"}
Then the response is 201 and the body contains the new widget's id
```

Each criterion maps 1:1 to a test. This is the lightweight core of spec-first work — just the
named outcomes for the change in front of you, not a 9-section document. It pays off three
ways: the criteria *are* the test list; vague asks get caught early ("should work well" is
untestable — rewrite it into a Given/When/Then or it isn't a criterion); and anything not
traceable to a criterion is scope creep you can drop.

**Stop and ask** instead of guessing when: the behavior for a case isn't determinable from
the request (don't invent it), the change would alter an existing API/DB contract, or it
touches auth/permissions/PII. Surface the ambiguity with a recommended option — don't encode
a silent guess into a test that then "passes."

## Part 2: Python Testing with pytest

pytest is the standard runner: plain `assert` statements, fixtures for setup/teardown
(`conftest.py` for sharing), `@pytest.mark.parametrize` for input matrices, and
`unittest.mock.patch` for boundaries (patch at the consumer module, not the definition;
mock more than 2 dependencies in one test is a red flag).

**Quick reference:**

| Need | Use |
|------|-----|
| Setup/teardown, shared data | `@pytest.fixture` (scopes: function/class/module/package/session) |
| Input matrix | `@pytest.mark.parametrize("a,b", [...])` |
| Isolate external calls | `@patch("consumer.module.func")`, `autospec=True` for signature safety |
| Expect an exception | `with pytest.raises(ValueError, match="...")` |
| Slow/optional tests | `@pytest.mark.slow`, `@pytest.mark.skipif(...)` |
| Async code | `@pytest.mark.asyncio` |

Full fixture/parametrize/mocking/async examples and `tests/` layout conventions:
`references/pytest-patterns.md`.

**Anti-patterns**: testing implementation instead of behavior, excessive mocking (mock
everything = test nothing), shared mutable state between tests, test interdependence,
ignoring failures instead of fixing or deleting the test.

## Part 3: API & Contract Testing

Unit tests prove a function behaves; **contract tests prove a boundary behaves** — the
request shapes a DRF endpoint accepts, the status codes it returns, the response shape callers
depend on, and the auth it enforces. This is where "unit tests green, integration broke"
comes from. Drive endpoint coverage from two matrices, not from the happy path:

- **Auth/permission matrix** — for every protected endpoint: missing header (401), invalid
  token (401), expired token (401), wrong role (403), not-owner (403), valid (2xx). Test
  "missing header" separately from "invalid token" — DRF routes them differently.
- **Input-validation matrix** — for every body-accepting endpoint: empty body, each required
  field missing, wrong type, boundary values (min−1/min/max/max+1), null. Bad input must
  return 400, never 500.

Use DRF's `APIClient` with `force_authenticate` in-process (transactional rollback per test,
no network); reserve `httpx` for cross-service or deployed-endpoint checks. Always assert the
response *shape* (the serializer is the contract) and that sensitive fields never leak — a
200 with the wrong body is still a bug. The same discipline extends to data-job
boundaries, where the input/output table schema is the contract.

Route-detection commands, the complete matrices, DRF + httpx patterns, and the
data-job boundary notes: `references/api-contract-testing.md`.

## Additional Resources

- `references/pytest-patterns.md` — fixtures, parametrize, mocking/patching, markers,
  exception/async tests, `tests/` layout.
- `references/api-contract-testing.md` — route detection, auth/input matrices, DRF +
  httpx patterns, data-job boundary contracts.
- `references/testing-anti-patterns.md` — mock-behavior-not-real-behavior pitfalls, test-only
  methods on production classes.
