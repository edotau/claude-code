# Quality Standards

Language specifics: `rules/software/<lang>/`.

## Rules

1. **Immutability (CRITICAL)** — return a new copy (`update(orig, k, v)`); never mutate in place.
2. **Handle every error** — user-friendly in UI code, detailed context server-side, never swallowed. Validate
   external input (user, API, file content) at the boundary and fail fast.
3. **Clean breaks in internal code** — a renamed command, flag or symbol changes outright, every caller fixed in the
   same pass; no back-compat aliases or shims. Published APIs still get a deprecation path.

## Code Quality Checklist

Functions < 50 lines, files < 800 (200–400 typical), nesting ≤ 4 (early returns); no hardcoded values, secrets, or
leftover debug prints. Over the cap, trim first: cut stacked comment blocks to one line that names what the code does.

## Comments

**1 line MAX, ≤120 chars, per section**, any language — code comments and docstrings alike, never a stacked block.
Exempt: provenance/audit blocks, file-header runbooks, gotcha notes. Prose over the cap is cut, not rewrapped: a
paragraph belongs in the repo's own docs (`docs/`, `ARCHITECTURE.md`, `README.md`), never in source.

## Simplify — the gate behind `skills/simplify`

`skills/simplify` (overrides the built-in `/simplify`) applies these rules as its apply-gate:

1. **Name the assumption before each cut** ("unreachable because ___"); can't finish the sentence → flag, don't cut.
2. **Never cut** semantic or gotcha comments, distinct test scenarios, error context (`%w`, messages), or a guard whose
   impossible state you can't prove.
3. **No covering test → flag, don't cut.** Gate: the scoped tests and live check pass before AND after; a cut near a
   test's subject re-runs `claude-code test mutate`.
4. **Reuse first**: grep the call sequence, sibling files and `git log -S`; a near-duplicate is compared with its
   original for behavior the copy lost (isolation, error handling), then consolidated at the source (say so).
5. **Altitude**: fix the root cause, not one more special case. Bugs found are reported, not fixed (`/code-review`).
6. **Thresholds**: this file's checklist (functions < 50 lines, nesting ≤ 4) and `review-simplicity`'s complexity
   > 10; input: `claude-code review simplify`. Under ~100 changed lines one in-context pass replaces the fan-out.
7. **Report numbers**: Δ lines and Δ complexity per touched function, plus what was skipped and why.

## Verification Order — lint first, always

| # | Step | When |
| --- | --- | --- |
| 1 | Parse/compile + **lint** (`ruff`, `go vet`, `make lint`/`fmt`) | Every edit, unprompted |
| 2 | **Live check** — run the verb, endpoint or flow on real input | Every change with a runnable surface, before any new test |
| 3 | The one thing changed — targeted assertion, negative control | Every edit |
| 4 | Scoped tests for the packages you touched, named explicitly | Once the direction settles, or before commit |
| 5 | **Simplify** — `Skill(simplify)` over the (merged) diff; re-run step 4 if it changed code | Every code change, before review, merge or commit — never skipped |
| 6 | Full suite + gate chain (format → lint → test → security) | **Once** at wrap-up, **only with the user's approval**, never a subagent |

Lint gates the rest: a syntax error makes test output meaningless. Untouched packages are noise during development.
Steps 1, 2, 5 and 6 are prose-enforced; hooks only format at Stop and give advisory review.
Report which steps ran and which did not. Judge a run by its own exit code (`set -o pipefail`), never `| tail`.

Live first, test scope, what makes a test a guard, and the test agent: `testing-guidelines.md`.

## Completion

Review done (`/code-review`), tests green, manual check for user-facing
changes, docs and links current; approve when no CRITICAL/HIGH remains. Severity scale:
`skills/evaluate-code/references/completion-criteria.md`.
