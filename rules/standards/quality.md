# Quality Standards

Language specifics: `rules/software/<lang>/`.

## Rules

1. **Immutability (CRITICAL)** — return a new copy (`update(orig, k, v)`); never mutate in place.
2. **Handle every error** — user-friendly in UI code, detailed context server-side, never swallowed. Validate
   external input (user, API, file content) at the boundary and fail fast.
3. **Clean breaks in internal code** — a renamed command, flag or symbol changes outright, every caller fixed in the
   same pass; no back-compat aliases or shims. Published APIs still get a deprecation path.

## Code Quality Checklist

Functions < 50 lines, files < 600 (200–400 typical), nesting ≤ 4 (early returns); no hardcoded values, secrets, or
leftover debug prints.

## Comments

**1 line MAX, ≤120 chars, per section**, any language — code comments and docstrings alike, never a stacked block.
Exempt: provenance/audit blocks, file-header runbooks, gotcha notes. Prose over the cap is cut, not rewrapped: a
paragraph belongs in the repo's own docs (`docs/`, `ARCHITECTURE.md`, `README.md`), never in source.

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
Report which steps ran and which did not. Judge a run by its own exit code (`set -o pipefail`), never `| tail`.

Live first, test scope, what makes a test a guard, and the test agent: `testing-guidelines.md`.

## Completion

Review done (`/code-review`; security lens for auth/input/API/crypto), tests green, manual check for user-facing
changes, docs and links current; approve when no CRITICAL/HIGH remains. Checklist and severity scale:
`skills/evaluate-code/references/completion-criteria.md`.
