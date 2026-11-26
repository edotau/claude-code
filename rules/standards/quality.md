# Quality Standards

Language specifics: `rules/software/<lang>/` (Go: `rules/harness/go.md`).

## Rules

1. **Immutability (CRITICAL)** — return a new copy (`update(orig, k, v)`); never mutate in place; language-specific
   rules win where idioms differ.
2. **Handle every error** — user-friendly in UI code, detailed context server-side, never swallowed. Validate
   external input (user, API, file content) at the boundary and fail fast.
3. **Clean breaks in internal code** — a renamed command, flag or symbol changes outright, every caller fixed in the
   same pass; no back-compat aliases or shims. Published APIs still get a deprecation path.

## Code Quality Checklist

Functions ≤ 50 lines, files ≤ 1200 (test files ≤ 1500), ≤ 25 imports per file, nesting ≤ 4 (early returns); no
hardcoded values, secrets or leftover debug prints. Enforced on new Go lines by `make lint` (`claude-code review
simplify --fail`), which also advises on retired words in added Go JSON tags and flag names without failing;
trim comments first.

**What the caps are for (owner, 2026-11-25):** they push toward reusable, stable code: write a function once,
stress-test it, then reuse it with minimal further tests. They are not there to limit complexity or file size for its
own sake, and a cap is never met by adding files, splitting a file, or wrapping code in thin helpers. Code near a cap is
a signal to look for what already does the job: fold duplicates, extend the closest existing function, delete what the
reuse makes dead. A new file needs a distinct role; a change reports its file count and net lines, and subtracts where
it can (less code, fewer tests, a smaller test bottleneck).

## Comments

**1 line MAX, ≤120 chars, per section**, any language, code comments and docstrings alike. Exempt: provenance/audit
blocks, file-header runbooks, gotcha notes. Prose over the cap is cut, not rewrapped; a paragraph belongs in the
repo's docs, never in source. Enforced by the same gate; whole-tree cleanup list: `claude-code review quality
--comments --format tsv <paths>`.

- **No narration** — nothing that restates the next line, the name or the signature. Comment only a why, a gotcha, a
  unit or an invariant the code cannot show.
- **Doc strings** — one line (≤ ~80 chars) only when it adds meaning; unexported symbols usually get none; never list
  parameters, returns or cases the code shows.
- **Shorten in place** — an over-long comment becomes one complete sentence that keeps its point; trim the verbose
  ones you pass.

Performance work does the same work faster and never drops a check, measured before and after (R22,
`standing-requirements.md`).

## Simplify — the gate behind `skills/simplify`

The apply-gate (assumption before each cut, never-cut list, reuse first, thresholds, numbers) lives in
`skills/simplify/SKILL.md`.

## Verification Order — lint first, always

| #   | Step                                                                                                                                                                          | When                                                                                                            |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| 1   | Parse/compile + **lint** (`ruff`, `go vet`, `claude-code agents diff --check --path <files>`) **and the IDE's diagnostics** (`mcp__ide__getDiagnostics` on each file touched) | Every edit, unprompted                                                                                          |
| 2   | **Live check** — run the verb, endpoint or flow on real input                                                                                                                 | Every change with a runnable surface, before any new test                                                       |
| 3   | The one thing changed — targeted assertion, negative control                                                                                                                  | Every edit                                                                                                      |
| 4   | Scoped tests for the packages you touched, named explicitly                                                                                                                   | In the sign-off driver (`worktree land`), not by a leaf; a leaf runs only the test it wrote or repairs          |
| 5   | **Simplify** — `Skill(simplify)` over the (merged) diff; re-run step 4 if it changed code                                                                                     | Every code change, and at the end of every wave over its merged diff                                            |
| 6   | Full suite + gate chain (format → lint → test → security)                                                                                                                     | **Once** at wrap-up, **only with the user's approval**; a leaf runs a chain target only when its brief names it |

`rtk make lint` and `agents diff --check` only check (a leaf runs `claude-code agents diff --check --path <files>` and
fixes its own files from the output, or with `claude-code agents fix --path <files>`); pass all your files in one
call (`--path a,b`, `--path a b` or repeated; a lane commit checks against main), never one call per file (each
re-runs the package tests); `rtk make fmt` fixes, `rtk make audit` and `rtk make install` ship from a clean
tree. A leaf may run them too; the audit after a worktree land runs on the lane's rebased head (HEAD plus the
landed diff) and at session end on clean main, automatically.

Never silence a checker (`noqa`, `type: ignore`, `pylint: disable`, wrapper tricks): refactor so the rule passes
(rare exception: the main session may ask the owner when the fix is disproportionate, one line vs a mountain of code;
agents never).
Lint gates the rest: a syntax error makes test output meaningless. Untouched packages are noise during development.
The IDE's diagnostics are a second gate, not a duplicate: the editor runs analyzers the repo's CLI lint does not
(Ruff rules, Pylance types, Pylint hints), so a file that is flake8/black clean can still show errors the user sees
— never call a file clean on CLI lint alone, and say which findings are pre-existing rather than silently fixing them.
Steps 1, 2, 5 and 6 are prose-enforced; hooks only format at Stop and give advisory review. Report which steps ran
and which did not. Judge a run by its own exit code (`set -o pipefail`), never `| tail`. Live first, test scope and
what makes a test a guard: `testing-guidelines.md`.

## Completion

Review done (`/code-review`), applicable scoped checks green, manual check for user-facing changes, docs and links
current; approve only at the R28 bar (severity scale:
`skills/evaluate-code/references/completion-criteria.md`). Only owner-judgment items (R25, R27) wait for the owner.
Anything noticed mid-task (a bug, an editor or lint finding, dead code) is fixed right away without asking,
within the named priority (R25).

**Review loop (R28):** every code change gets an other-family review that lists every finding at once; the reviewer
may fix a finding itself in the lane (it then lists each edit), and the author fixes the rest, any severity, without
asking; the review repeats until one finds nothing. Three rounds in a
row that each find a new HIGH in the same area stop the loop: bring the pattern and a design recommendation to the
owner. A lane merges when the last review has no CRITICAL or HIGH; anything it still finds is fixed in a follow-up
commit on main, never parked.
Findings are fixed, not reported: a leaf hands back only what it could not fix, as one final `UNFIXED: none` or
`UNFIXED: <item>; <item>` line, and the main session fixes or decides that at once (its recommendation is the
decision; it never asks the owner) and relays only the outcome.
Every dispatched code task carries this goal (`agents ask` adds it to every brief that can write code; a
review-only task reports findings instead).

**Broken tests (R29):** when a change breaks a test, a wrong test is repaired at once by the same agent when small, else
by a test-repair leaf (`skills/subagent-workflows/SKILL.md`); a real regression goes back to the implementer or a
code-fix leaf; a leaf runs only the test it repairs, by exact name. A test failing in the collector is triaged by the
main session: a minor cause it fixes itself; a real defect goes back to the lane or a new subagent with the failure
output; after a repair the main session reruns every package the repair or the failing lanes touched. Never delete or
skip a test; under heavy concurrent load the repair queues.
