# Test Discipline

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
makes stale is not "writing tests" and proceeds concurrently as usual (fan out the `copilot` agent); only NEW
tests wait for the live check.

### Where nothing can be run live

Pin the contract from the spec as Given/When/Then, write the test against the code under test (it must
import it), then prove its teeth by revert-and-restore. Verify coverage (80%+). How-to: the
`testing-guidelines` skill.

Reach for **copilot** once the live check has landed, or immediately where there is no surface to
run. On failures: check test isolation, verify mocks, fix the implementation (not the test, unless the
test is wrong).

### Test-agent concurrency

Code → fix the broken tests → write new tests is **three serial passes over one change**: the
biggest time sink in a coding task, and the reason collateral breakage always surfaces last.
Concurrency is the whole point of the rule; a test agent dispatched after the code lands
satisfies the letter and none of the value.

The concurrent agent is a **repair** agent (`copilot`) — existing tests the change breaks, which need
no live check. NEW coverage is still gated on the live run above, so it is dispatched after it, not here.

**Ownership split** (no overlap, so the agents don't fight over the same files):

| Who | Owns |
|-----|------|
| Main thread | The **live check** on the real surface, before any new test — never delegated. Only where nothing can be run live does this become a spec-pinned test for the core contract. |
| Implementation agent(s) | The code change. |
| Test agent(s) | Everything else about tests: new coverage (edge cases, table rows, regression guards) **and** the EXISTING tests the change breaks or makes stale. Sized to the change, written after the live check, run scoped to the touched packages. |

**Sizing.** One test agent minimum; scale with the implementation fan-out (roughly one per
distinct suite or module under change). Test agents are lean leaves — `copilot`, or
`code-worker` for a scoped suite — so they sit outside the ≤8 deep-context tier.

**Not satisfied by:**

- One agent told to "implement it and write tests" — that serializes inside a single context, and
  the tests land last or not at all. Separate agents, same message.
- A test agent dispatched once the implementers return. That IS the serial pass this forbids.
- "The change is too small." Speed is the default: the main thread plans and stays free while
  `code-worker` ‖ `copilot` run in the background (CLAUDE.md guideline 8). Only a one-line edit goes inline.

**Genuinely out of scope:** non-code dispatches (exploration, research, docs, review-only), and a
target with no test suite — there the test agent's job is to report that, not to invent a framework
unasked.

Report the remainder as a list, never as silence: tests planned but not written are an outcome, not
an omission to leave undiscovered.

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
