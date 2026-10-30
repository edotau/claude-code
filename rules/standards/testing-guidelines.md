# Testing Guidelines

Order, always: **run it live → pin what you observed → prove the test bites.** Gate order (lint → live → scoped →
full suite once): `quality.md` → Verification Order. Optional test-and-fix loops (parallel and concurrent) and tooling:
`skills/subagent-workflows/references/test-and-fix.md`.

## Live first, then tests — no core-contract exception

**Exercise the change on the real surface before writing a single NEW test.** When the task has a runnable surface —
a verb, a hook, an endpoint, a client request, a make target — run it on the real case (real service, input, bytes)
and SHOW the output. No new test until that output confirms the change does what you believe. There is no
core-contract carve-out: a RED-first test written before the live run pins your GUESS at the behavior, and a fix
whose only evidence is a test written alongside it is unverified.

- **A bug fix's live run IS the RED:** reproduce the defect on the real surface, state the root cause ("X because
  Y"), fix, re-run the same command; the before/after output is the evidence.
- **Repairing EXISTING tests** the change breaks is not "writing tests" — it runs concurrently (`copilot`). Only NEW
  tests wait for the live check.
- **No real input available:** say so and stop. Ask for the input, naming exactly what's needed; mark anything
  already written as unverified. More tests are not a substitute.
- **Where nothing can be run live:** pin the contract from the spec as Given/When/Then, write a test that imports the
  code under test, and prove its teeth (below).
- **The tell you skipped it:** the code has never consumed a real input and the test count is climbing, or a memo
  states conclusions no execution produced.

## Scope — logic, not a coverage number

Per feature, one test spanning its functions end to end plus the corner cases known while building it; per caught
bug, one regression test that fails without the fix. Nothing more unless asked — no coverage waves, no exhaustive
matrices. Renames, moves and deletions need no new tests. No fixtures for states the code can no longer produce: a
refactor that breaks only tests means rewrite them against behavior.

## A test must guard

- **Import the code under test.** A test that rebuilds the logic inline passes with the fix reverted — documentation,
  not a guard (`grep -nE '^(import|from)' <test> | grep <module>`).
- **Prove the teeth, don't reason about them:** `claude-code test mutate <file> <old> <new> [--run RE] <pkg>` (exit 0
  = the suite failed with the defect in). Never edit-and-restore a tracked file; confirm the failure message matches
  the defect. A test that cannot fail when the code is wrong is deleted.
- **Reproduce the whole fault**, not part of it — build the defect's worst case, or the guard passes over it.
- **Equivalence tests assert the fast path ran** (a file it requests, a call count, a cache key); stub the shared
  dependency with a deterministic oracle so any difference is the logic under test.
- **Derive expected values from the fixture**, never hardcode a belief about it; drive parsers with real bytes
  sampled from the live artifact.
- **Mock only real boundaries** — network, model calls, the clock, external CLIs — at the consumer's import site;
  everything else runs real code against a hermetic config dir. More than two mocks in a test is a missing seam;
  asserting on a mock's own behavior fails the import rule.
- **Stop and ask** instead of encoding a guess when a case's behavior isn't determinable from the request or data,
  the change alters an API/DB contract, or it touches auth/permissions/PII.

## Test agent — only when the change needs one

Dispatch it in the SAME message as the implementer only when the change **adds a feature** or **fixes a bug**; a
rename, move, deletion or refactor gets none (the implementer keeps the build and existing tests green). Code → fix
broken tests → write new tests is three serial passes; concurrency is the point, so a test agent dispatched after the
implementers return satisfies the letter and none of the value — as does one agent told to "implement and test".

| Who | Owns |
| --- | --- |
| Main thread | The live check on the real surface — never delegated |
| Implementer(s) | The code change |
| Test agent(s) | Existing tests the change breaks (at once) and new coverage (after the live check), scoped to touched packages |

- **Handoff, not polling:** the test agent takes the baseline, reads the code, ends its turn; the manager
  `SendMessage`s "landed" with files and signatures, which resumes it. In a Workflow, `pipeline` orders it.
- **Sizing:** one minimum, roughly one per distinct suite under change; `copilot`, or `code-worker` for a scoped
  suite; they count toward the dispatch cap (`rules/harness/hooks.md`).
- **Out of scope:** exploration, research, docs and review-only dispatches, and a target with no suite (report that;
  don't invent a framework). Tests planned but not written are reported as a list, never as silence.
