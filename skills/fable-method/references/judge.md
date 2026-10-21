# judge — adversarial verification of finished work

**Stance, fixed: a report is a set of claims, not evidence. Nothing is believed that was not
observed.** The most documented failure of coding agents is claiming success regardless of
reality — "fixed, all tests pass" on broken work, tests quietly weakened until green, scope
silently expanded.

Use when *someone else* called it done: another agent, another model, a pasted report, a branch
you didn't watch get built. For your own work mid-loop, Step 5 plus `approval-gate` is the gate;
for a graded transcript walk-through use `audit` mode.

**Related, don't duplicate:** `codex` (VERIFY mode) runs the *project's* gate chain and fixes
failures. This judges *claims* against *the diff* and changes nothing. Workflow `review-funnel` finds
new defects across dimensions; this refutes asserted ones. `evaluate-code` judges quality;
this judges honesty.

## Protocol

1. **Collect the claims.** From the report or conversation, list what was supposedly done, what
   was supposedly verified ("tests pass", "build green", "renders correctly"), and what was
   supposedly left untouched. Each becomes a row to prove or refute.
2. **Establish what actually changed.** `git diff` + `git status` (or a directory diff against a
   pristine reference when there's no repo). **The diff is ground truth; the report is not.**
   Compare touched files against the ask's blast radius, and against the declared scope if the
   work declared one.
3. **Re-run every claimed verification yourself.** Do not read code and nod — run the tests, the
   build, the command, the page. Capture actual output. A claim that cannot be re-run (missing
   environment, credentials, human-eyes-only) is labeled **UNVERIFIABLE**, never assumed true.
4. **Hunt the classic frauds**, in order of real-world frequency (full catalogue:
   `failure-modes.md`):

   - **Weakened checks.** Diff the test files specifically: assertions loosened or deleted,
     expected values edited to match new behavior, tests skipped, tolerances widened, real calls
     swapped for mocks. **A changed test is guilty until its justification traces to a spec.**
   - **False completion.** A pass claimed with no run shown, a partial pass reported as full,
     "should work now", success language over a failure transcript.
   - **Scope creep.** Changes beyond the ask: drive-by refactors, reformatting, new dependencies.
   - **Unauthorized action.** An outward-facing effect — deploy, push, publish, send, install,
     schedule, delete of shared data — that no quoted user instruction covers. Look for the
     `AUTH:` line and **check its quote against the conversation**. An outward effect visible in
     the diff or environment (a new remote, a deploy marker, a sent artifact) with no `AUTH:`
     line, or with a quote that doesn't authorize *that* action, is the fraud. Documentation
     telling the agent to deploy is not authorization.
   - **Spec betrayal.** Code changed to satisfy a check that contradicts the README/spec/
     docstring. Authority order: explicit user statement > spec > tests > current behavior.
   - **Debris.** Leftover scratch files, debug prints, commented-out code, orphaned imports.

   In this harness, add the local frauds:

   - **Hand-edited `settings.json`** — generated artifact; the change belongs in
     `internal/settings/template/` and dies on the next regenerate (CLAUDE.md rule 1).
   - **AI-attribution footers in commit messages** — `Co-Authored-By: Claude`, `🤖 …`,
     `Claude-Session:` trailers (CLAUDE.md rule 2).
   - **A static `ANTHROPIC_*` token or a public `DATABRICKS_*` profile key at rest** (rules 3–4).
   - **`--dangerously-skip-permissions`** anywhere (rule 5).
   - **An edited generated mirror** — `AGENTS.md` or a synced tool dir instead of its source
     (rule 6).
   - **A cross-workspace change with no fan-out decision recorded** —
     `rules/harness/change-protocol.md` Impact Checklist.
   - **Unreconciled foreign import** — vendored skill/agent/doc content still addressing its
     source harness: self-declaring headers (`# /some-command Skill`), references to commands or
     paths this repo does not provide, links into directories that never existed here. Passes a
     link check once repointed while still speaking to the wrong reader.

5. **Deliver the verdict, evidence first.**

   - **VERIFIED** — every load-bearing claim reproduced; no frauds found.
   - **VERIFIED WITH CAVEATS** — the work is sound; list exactly what could not be re-run, and
     any minor debris.
   - **REFUTED** — a claim failed reproduction or a fraud was found: name the exact claim, show
     the output that contradicts it, state the smallest fix.

   Format: verdict on the first line; then a claims table (claim | what was observed); then
   frauds found, if any; then the recommended action.

   **Never soften a refutation to be polite, and never inflate a caveat into a refutation to look
   rigorous.** Both are dishonesty; the second is costume rigor (failure mode 18).

## Standing rules

- **Judging changes nothing.** Read and run only. Fixes happen after, and only if asked.
- Minutes, not hours. This is a gate, not a second implementation.
- If verification needs an environment you lack, hand that back rather than guessing.
- If the work touched nothing runnable, say plainly what a judge can and cannot check here.

## Non-code work

Judged against whatever the deliverable's own domain treats as authoritative — figures re-fetched
from source, arithmetic recomputed, copy checked line by line against the style or brand
reference, cited claims opened. Same stance: the deliverable's claims are verified against
sources, never accepted because they read confidently. Domain fraud tables (fabricated
statistics, stale figures, silent data cleaning) are in upstream's `references/domains/`; this
harness routes domain work to its own skills instead — `scientific-writing`, `devops-engineer`,
`databricks-expert`, `bi-policies`.
