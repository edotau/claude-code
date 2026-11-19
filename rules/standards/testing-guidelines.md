---
paths:
  - "**/*_test.go"
  - "**/test_*.py"
  - "**/*_test.py"
  - "**/*.test.ts"
  - "**/*.spec.ts"
  - "**/tests/**"
  - "**/testdata/**"
---

# Testing Guidelines

Order, always: **run it live → pin what you observed** (gate order: `quality.md` → Verification Order).

## Live first, then tests

**Run the change on its real surface (verb, hook, endpoint, make target) with real input and show the output before
writing any NEW test.** A test written first pins your guess, not the behavior.

- **A bug fix's live run is the RED:** reproduce it, state the root cause ("X because Y"), fix, re-run the same
  command; the before/after output is the evidence.
- **Repairing existing tests** the change breaks is not new testing; the implementer or a repair leaf does it.
- **No real input:** say so, ask for exactly what is needed, and mark anything written as unverified.
- **No runnable surface:** pin the spec as Given/When/Then in a test that imports the code.

## Scope — logic, not a coverage number

Default per feature: basic tests that it works (a couple of seconds to run at most) before it has been used a while;
corner cases and per-bug regression tests are proposals (unneeded tests cost repair time); a change that needs more up
front (a security boundary, a data-loss path) says why in the hand-back. Renames, moves and deletions need none; a
refactor that breaks only tests means rewriting them against behavior, not keeping fixtures for states the code can no
longer produce. A test `review-simplicity` flags (complexity > 10, > 50 lines) tests too much at once: cut or split
it, never raise the limit.

## A test must guard

- **Teeth, proven** — call the code under test, never an inline copy of its logic. A new test must fail with the
  guarded behavior removed: flip it, watch it fail, restore the source exactly, and report that you did. A test that
  still passes is deleted.
- **Shapes and edges, not values** — build the defect's worst case (empty, one element, the boundary, malformed,
  concurrent, present-but-empty); derive expected values from the fixture and drive parsers with real bytes. Never pin
  a config value, a help string or a generated file's bytes. Equivalence tests assert the fast path actually ran (a
  call count, a cache key) against a deterministic oracle.
- **Mock only real boundaries** (network, model calls, clock, external CLIs, process scans), at the consumer; run
  everything else against a hermetic config dir (`t.TempDir()`, `CLAUDE_CONFIG_DIR`). More than two mocks is a missing
  seam; in Go the seam is a package var, swapped and restored with `t.Cleanup`.
- **Fast first** — a new test runs in milliseconds and its package gets at most 0.5 s slower (measure before and
  after): no sleep, real network, vault, daemon or spawned binary, and never the test binary (`os.Executable()` is
  `go test`'s; stub every daemon-spawn seam). Shrink timeouts through package vars and seed fixtures directly.
- **An agent's or workflow's test stage writes only** repairs of tests its edits broke, plus one new test per major
  feature. Every other test is proposed in the final report (the case, what it guards, its runtime) for the owner to
  decide; judge proposals by speed first, then teeth, then no overlap. The implementer writes its feature's basic tests.
- **Fake credentials and hosts look nothing like real ones** — a token, key or secret fixture never starts with a vendor
  prefix (`dapi`, `dose`, `ghp_`, `github_pat_`, `sk-`, `tskey-`, `AKIA`, `xox`, `eyJ` …) or matches its length and
  alphabet: scanners such as GitHub's flag a fake as an exposed secret. Use plain gibberish with no prefix (a random
  mash of letters, a new one per test); prefer tests that never need a credential's shape. When the code under test
  checks the shape, build the value at runtime from the code's own prefix constant plus filler (`prefix +
  strings.Repeat("x", n)`), so no literal is in the source; keep such tests few and shallow. Hosts are made-up names
  (`devhost`) on documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), never a real
  host name, login, tailnet name or address.
- **At a boundary, error paths first**, asserting shape (required fields present, sensitive ones absent); a crash or
  500 on bad input is a finding.
- **Stop and ask** rather than encode a guess when behavior isn't determinable, or the change touches an API/DB
  contract, auth, permissions or PII.

## When a test fails

The test is guilty until its expectation traces to the spec, the request or observed behavior — say which. Rerun a
red seen under parallel load solo before calling it a regression. Never weaken an assertion, skip a test or edit a
migration to go green; after 3 fix attempts on one test, report it.
