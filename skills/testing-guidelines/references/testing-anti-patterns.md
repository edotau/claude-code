# Testing Anti-Patterns

**Load this reference when:** writing or changing tests, adding mocks, or tempted to add test-only
methods to production code.

**Core principle:** Test what the code does, not what the mocks do.

## Mock at boundaries, not everywhere

Mock external IO at the consumer's import
site, mirror the real object completely, and treat more than two mocks in one test as a design
smell (the code is too coupled — add a seam). Never add test-only methods to production
classes. Asserting on a mock's own behavior fails the import rule in disguise: the code under
test never ran. Full pitfalls and gate checks: `references/testing-anti-patterns.md`.

**Running the code on real input first, then proving each test's teeth, prevents these
anti-patterns** (see `../SKILL.md`).

## The Iron Laws

```text
1. NEVER test mock behavior
2. NEVER add test-only methods to production classes
3. NEVER mock without understanding dependencies
```

## Anti-Pattern 1: Testing Mock Behavior

Before: `assert find_by_test_id("sidebar-mock").exists` — proves the mock rendered, not that
the component works.
After: `assert find_by_role("navigation").exists`, sidebar unmocked — proves real behavior.
Gate: before asserting on any mock element, ask "real behavior or just mock existence?"; if the
latter, delete the assertion or unmock the component.

## Anti-Pattern 2: Test-Only Methods in Production

Before: `Session.destroy()` added to production only so `tearDown` can call it.
After: a `cleanup_session(session)` helper in test utilities; `Session` stays stateless.
Gate: before adding a method to a production class, ask "only used by tests?" — if yes, it
belongs in test utilities, not the class.

## Anti-Pattern 3: Mocking Without Understanding

Before: mocking `discover_and_cache_tools` silently skips the config write a later assertion
in the same test depends on, so a duplicate-detection check never fires.
After: mock only the slow/external call (server startup); keep the write path real.
Gate: before mocking any method, ask what side effects the real method has and whether the
test depends on them; if unsure, run the real implementation first and observe.

## Anti-Pattern 4: Incomplete Mocks

Before: `mock_response = {status, data}` — missing the `metadata` field downstream code reads.
After: `mock_response = {status, data, metadata}` — mirrors the complete real API shape.
Gate: before creating a mock response, check what fields the real API returns and include all
of them, not just the ones the immediate test touches.

## Anti-Pattern 5: Integration Tests as Afterthought

Before: "implementation complete, no tests written, ready for testing."
After: run on real input, pin the behavior in a test that imports the code, prove it bites.
Gate: testing is part of implementation, not an optional follow-up — don't claim complete
without it.

## Quick Reference

| Anti-Pattern | Fix |
| -------------- | ----- |
| Assert on mock elements | Test real component or unmock it |
| Test-only methods in production | Move to test utilities |
| Mock without understanding | Understand dependencies first, mock minimally |
| Incomplete mocks | Mirror real API completely |
| Tests as afterthought | Live run, then guard tests, before claiming complete |
| Over-complex mocks | Consider integration tests |

## The Bottom Line

**Mocks are tools to isolate, not things to test.** If the teeth proof shows you're testing mock
behavior, fix it: test real behavior, or question why you're mocking at all.
