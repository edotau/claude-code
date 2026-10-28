# Quality Standards

Code quality, immutability, testing, and completion criteria. Language specifics:
`rules/software/<lang>/coding-style.md` + `testing.md`.

## Core Principles

1. **Quality-first completion** — zero-defect handoff: no work is "complete" with known
   quality issues. All acceptance criteria verified through testing, not assertion.
2. **Immutability (CRITICAL)** — create new objects, never mutate in place. Immutable
   data prevents hidden side effects and enables safe concurrency.

   ```text
   WRONG:   modify(original, field, value)  → changes original in place
   CORRECT: update(original, field, value)  → returns a new copy
   ```

3. **Explicit error handling** — handle errors at every level; user-friendly messages in
   UI-facing code; detailed context server-side; never silently swallow.
4. **Validate at boundaries** — validate all external input (user, API responses, file
   content) with schema-based validation where available; fail fast with clear messages.
5. **Clean breaks over back-compat cruft (internal code)** — when renaming a command,
   flag, or symbol in a self-contained repo (no external consumers), change it outright and
   fix every caller in the same pass. Do **not** leave back-compat aliases/shims "just in
   case" — they accumulate as dead weight. If the rename breaks something, fix it
   immediately. (External/published APIs still warrant a deprecation path — this is for
   in-repo churn only.)
6. **Existing descriptions are curated — leave them as written** — when adding a tool, target,
   flag, or command, write only the NEW entry's description. Do not rename, reword, or "align"
   prior descriptions, help text, comments, or surrounding context, and do not add, drop, or
   swap fields in an existing output block (e.g. a status printout); they were written to the
   user's style and already reviewed. Fix only bugs (wrong/stale facts), spelling, and light
   grammar. If a prior description genuinely needs more than that, ask first.

## Code Quality Checklist

Before marking work complete:

- [ ] Readable and well-named
- [ ] Functions small (< 50 lines)
- [ ] Files focused (< 800 lines; 200–400 typical) — many small files over few large
- [ ] No deep nesting (> 4 levels) — use early returns
- [ ] Proper error handling; no bare excepts / swallowed exceptions
- [ ] No hardcoded values (constants or config)
- [ ] No mutation (immutable patterns)
- [ ] No secrets, no debug/print statements left in

## Verification Order — lint first, always

**Run the linter/formatter before any other verification, on every edit.** It is the cheapest signal
and it gates the expensive ones: a syntax or import error makes a test run's output meaningless, and a
formatter diff buried under test failures gets fixed twice.

| Order | Step | When |
| --- | --- | --- |
| 1 | Parse/compile check + **lint** (`ruff`, `go vet`, `make lint`/`fmt`) | **Every** edit, unprompted |
| 2 | **Live check** — run the verb, command, endpoint or flow the task is about, on real input | Every change that has a runnable surface, BEFORE any NEW test is written |
| 3 | The one thing changed — the edited symbol, a targeted assertion, a negative control | Every edit |
| 4 | Scoped tests for the packages/modules you touched — **only** those; subagents the same | Once the direction settles, or before commit |
| 5 | Full suite + gate chain (format -> lint -> test -> security) | **Once**, at session wrap-up, **only with the user's approval** (never a subagent) |

Steps 1-3 are always yours to run. Step 4 stays scoped: a package you did not touch is noise during
development, and every scoped run must name its packages. Step 5 is the one full run per delivery —
never per increment, never unprompted (`CLAUDE.md` → Behavioral guidelines): during an interactive back-and-forth
a suite run per increment verifies code the next instruction may replace. Say which steps ran and which
did not, so the gap is visible.

## Testing

**Tests verify logic, not a coverage number.** Per feature, the minimum is one test that spans the functions it
involves end to end; then the corner cases known while building it; then, as bugs are caught, one regression test
per bug that fails without the fix. Nothing more unless asked — no coverage waves, no exhaustive matrices.

- **Mock only real boundaries** — network, model calls, the clock, external CLIs (`gh`, `databricks`). Everything else
  runs real code against a hermetic config dir.
- **No fixtures for states the code can no longer produce.** A refactor that breaks only tests means the tests were
  coupled to structure: delete or rewrite them against behavior, don't repair them.
- **A test that cannot fail when the code is wrong is deleted**, not kept.
- **Renames, moves and deletions need no new tests** — the build and the existing behavior tests are the check.

Live first, then tests: [procedure][proc].

Where nothing can be run live: [spec-pinned checks][spec-pinned-checks].

### RULE — a test agent only when the change needs one

Dispatch a test agent alongside the implementer, in the SAME message, only when the change **adds a feature** (it
writes the one spanning test plus known corner cases) or **fixes a bug** (the regression test). A rename, move,
deletion or refactor gets none: the implementer keeps its own build and existing tests green.

Ownership split: [ownership table][ownership-table].

Handoff: the test agent waits for the manager's "landed" `SendMessage`, never a polling loop — [handoff][handoff].

Sizing: [test-agent sizing][test-agent-sizing].

Not satisfied by: [invalid dispatch patterns][invalid-dispatch-patterns].

Genuinely out of scope: [exceptions][exceptions].

## Completion Criteria (all tasks)

Acceptance criteria verified by observation, not assertion; review done (`/code-review`, with the security lens
for auth/input/API/crypto); automated tests green and manual checks for user-facing changes; docs and links current.
Full checklist and the severity scale (CRITICAL → LOW; approve when no CRITICAL/HIGH):
`skills/evaluate-code/references/completion-criteria.md`.

Fixture discipline and mutation (injection) proofs: the `testing-guidelines` skill.

[proc]: ../../skills/testing-guidelines/references/test-discipline.md#live-first-then-tests--no-core-contract-exception
[spec-pinned-checks]: ../../skills/testing-guidelines/references/test-discipline.md#where-nothing-can-be-run-live
[ownership-table]: ../../skills/testing-guidelines/references/test-discipline.md#ownership-split
[handoff]: ../../skills/testing-guidelines/references/test-discipline.md#handoff-not-polling
[test-agent-sizing]: ../../skills/testing-guidelines/references/test-discipline.md#sizing
[invalid-dispatch-patterns]: ../../skills/testing-guidelines/references/test-discipline.md#not-satisfied-by
[exceptions]: ../../skills/testing-guidelines/references/test-discipline.md#genuinely-out-of-scope
