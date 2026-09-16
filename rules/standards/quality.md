# Quality Standards

Code quality, immutability, testing, and completion criteria. Language specifics:
`rules/software/<lang>/coding-style.md` + `testing.md`.

## Core Principles

1. **Quality-first completion** — zero-defect handoff: no work is "complete" with known
   quality issues. All acceptance criteria verified through testing, not assertion.
2. **Immutability (CRITICAL)** — create new objects, never mutate in place. Immutable
   data prevents hidden side effects and enables safe concurrency.
   ```
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
| 5 | Full suite + gate chain (format -> lint -> test -> security) | **Once**, when the app is complete and about to build and push |

Steps 1-3 are always yours to run. Step 4 stays scoped: a package you did not touch is noise during
development, and every scoped run must name its packages. Step 5 is the one full run per delivery —
never per increment, never unprompted (`CLAUDE.md` → Behavioral guidelines): during an interactive back-and-forth
a suite run per increment verifies code the next instruction may replace. Say which steps ran and which
did not, so the gap is visible.

## Testing

**Minimum coverage: 80%.** Three test types, all required:

1. **Unit** — individual functions, utilities, components
2. **Integration** — API endpoints, database operations
3. **E2E** — critical user flows (framework per language)

### Live first, then tests — no core-contract exception

**Exercise the change on the real surface before writing a single NEW test.** When the task has a
runnable surface — a verb, a hook, an endpoint, a client that makes a request, a make target — run it on
the real case (real service, real input, real bytes) and SHOW the output. No new test is written until
that output confirms the change does what you believe it does. There is no core-contract carve-out: a
RED-first test written before the live run pins your GUESS at the behavior, and a fix whose only evidence
is a test you wrote alongside it is unverified. Tests come AFTER, sized to the change — a one-function fix
gets the assertion that pins it, not a table of every neighbour.

For a bug fix the live run IS the RED: reproduce the defect on the real surface first, fix, re-run the
same command, and the before/after output is the evidence. Repairing EXISTING tests the change breaks or
makes stale is not "writing tests" and proceeds concurrently as usual (fan out `test-repair`); only NEW
tests wait for the live check.

### TDD cycle — only where nothing can be run live

```
1. Write the test first        (RED)
2. Run it — it must FAIL
3. Write minimal implementation (GREEN)
4. Run it — it must PASS
5. Refactor                     (IMPROVE)
6. Verify coverage (80%+)
```

Reach for **test-driven-dev** once the live check has landed, or immediately where there is no surface to
run. On failures: check test isolation, verify mocks, fix the implementation (not the test, unless the
test is wrong).

### RULE — every code dispatch carries a test agent, concurrently

**Any dispatch of agents to write or change code MUST include at least one agent owning tests,
launched in the SAME message as the implementation agent(s).** Not after they return; not "if
there's time". One code agent still means one test agent — the minimum is one, not zero.

Code → fix the broken tests → write new tests is **three serial passes over one change**: the
biggest time sink in a coding task, and the reason collateral breakage always surfaces last.
Concurrency is the whole point of the rule; a test agent dispatched after the code lands
satisfies the letter and none of the value.

The concurrent agent is a **repair** agent (`test-repair`) — existing tests the change breaks, which need
no live check. NEW coverage is still gated on the live run above, so it is dispatched after it, not here.

**Ownership split** (no overlap, so the agents don't fight over the same files):

| Who | Owns |
|-----|------|
| Main thread | The **live check** on the real surface, before any new test — never delegated. Only where nothing can be run live does this become a RED-first test for the core contract. |
| Implementation agent(s) | The code change. |
| Test agent(s) | Everything else about tests: new coverage (edge cases, table rows, regression guards) **and** the EXISTING tests the change breaks or makes stale. Sized to the change, written after the live check, run scoped to the touched packages. |

**Sizing.** One test agent minimum; scale with the implementation fan-out (roughly one per
distinct suite or module under change). Test agents are lean leaves — `test-driven-dev`,
`test-repair`, or `code-workers` for a scoped suite — so they sit outside the ≤4 deep-context tier.

**Not satisfied by:**

- One agent told to "implement it and write tests" — that serializes inside a single context, and
  the tests land last or not at all. Separate agents, same message.
- A test agent dispatched once the implementers return. That IS the serial pass this forbids.
- "The change is too small." A change too small for a test agent is too small for a dispatch —
  do it inline.

**Genuinely out of scope:** non-code dispatches (exploration, research, docs, review-only), and a
target with no test suite — there the test agent's job is to report that, not to invent a framework
unasked.

Report the remainder as a list, never as silence: tests planned but not written are an outcome, not
an omission to leave undiscovered.

## Completion Criteria (all tasks)

- [ ] All acceptance criteria satisfied with documented verification
- [ ] Edge cases and error scenarios tested
- [ ] Integration points validated
- [ ] Code review completed (`/code-review` or the **evaluate-code** skill; the **security-reviewer**
      skill via `/security` for auth/input/API/crypto)
- [ ] Automated tests passing; manual testing done for user-facing changes
- [ ] Documentation updated; no broken links or missing references

## Review Severity

| Level | Meaning | Action |
|-------|---------|--------|
| CRITICAL | Security vuln or data-loss risk | **BLOCK** — must fix |
| HIGH | Bug or significant quality issue | **WARN** — should fix |
| MEDIUM | Maintainability concern | **INFO** — consider |
| LOW | Style / minor | **NOTE** — optional |

Approve when no CRITICAL/HIGH. Block on CRITICAL/HIGH.

## Fixtures From the Live Artifact

Drive parsers with real bytes sampled from the artifact they parse, not synthetic examples. Real headers
range-read from five live databases immediately exposed two defects that hand-written fixtures had hidden
for months (an entry separator that made one field swallow the next, and a rebuilt DB whose headers
parsed to null). A synthetic fixture encodes the author's belief about the format; a sampled one does not.

## Name the Gates You Ran

A formatter + tests is not the gate chain when the project's lint target also runs a type checker
(pyright, mypy, `go vet`). State which gates ran and which did not -- a type error is invisible to the
linter that passed.

## Prove the Injection Before Trusting a Teeth Proof

Revert-and-restore ("break the code, confirm RED, restore") is only evidence if the break ACTUALLY LANDED.
A `python -c` one-liner with nested quotes, or a `str.replace` whose anchor has since been reformatted,
silently changes nothing — and the tests then pass for the very reason you were trying to disprove. That is
worse than no proof: it manufactures false confidence.

Assert the mutation, every time: `assert s.count(old) == 1` before writing, then `grep` the injected marker
out of the file on disk and show it. If the anchor is not found, the run must FAIL loudly, not continue.
Formatters are the usual culprit — `black`/`ruff format` runs after an edit, so an anchor copied from an
earlier read may no longer exist.

