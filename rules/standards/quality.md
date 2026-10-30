# Quality Standards

Language specifics: `rules/software/<lang>/`.

## Principles

1. **Done means verified** — no known issues at handoff; acceptance criteria proven by observation, not assertion.
2. **Immutability (CRITICAL)** — return a new copy (`update(orig, k, v)`); never mutate in place.
3. **Handle every error** — user-friendly in UI code, detailed context server-side, never swallowed. Validate
   external input (user, API, file content) at the boundary and fail fast.
4. **Clean breaks in internal code** — a renamed command, flag or symbol changes outright, every caller fixed in the
   same pass; no back-compat aliases or shims. Published APIs still get a deprecation path.
5. **Existing descriptions are curated** — when adding an entry (tool, flag, command, target), write only its text.
   Don't reword or realign existing descriptions, help text or comments, or add/drop/swap fields in an existing
   output block. Fix only wrong facts, spelling and light grammar; ask before anything more.

## Code Quality Checklist

Functions < 50 lines, files < 800 (200–400 typical), nesting ≤ 4 (early returns); no hardcoded values, secrets, or
leftover debug prints.

## Verification Order — lint first, always

| # | Step | When |
| --- | --- | --- |
| 1 | Parse/compile + **lint** (`ruff`, `go vet`, `make lint`/`fmt`) | Every edit, unprompted |
| 2 | **Live check** — run the verb, endpoint or flow on real input | Every change with a runnable surface, before any new test |
| 3 | The one thing changed — targeted assertion, negative control | Every edit |
| 4 | Scoped tests for the packages you touched, named explicitly | Once the direction settles, or before commit |
| 5 | Full suite + gate chain (format → lint → test → security) | **Once** at wrap-up, **only with the user's approval**, never a subagent |

Lint gates the rest: a syntax error makes test output meaningless. Untouched packages are noise during development.
Report which steps ran and which did not.

## Testing

Tests verify logic, not a coverage number: per feature, one test spanning its functions end to end plus the corner
cases known while building it; per caught bug, one regression test that fails without the fix. Nothing more unless
asked.

- Mock only real boundaries — network, model calls, the clock, external CLIs; everything else runs real code
  against a hermetic config dir.
- No fixtures for states the code can no longer produce; a refactor that breaks only tests means rewrite them
  against behavior.
- A test that cannot fail when the code is wrong is deleted. Renames, moves and deletions need no new tests.

## Test agent — only when the change needs one

Dispatch it in the SAME message as the implementer only when the change **adds a feature** (the spanning test +
corner cases) or **fixes a bug** (the regression test). Rename, move, deletion or refactor: none — the implementer
keeps the build and existing tests green. The test agent waits for the manager's "landed" `SendMessage`, never a
polling loop. Ownership split, sizing, invalid patterns, exceptions, no-live-surface checks:
`skills/testing-guidelines/references/test-discipline.md`.

## Completion

Review done (`/code-review`; security lens for auth/input/API/crypto), tests green, manual check for user-facing
changes, docs and links current; approve when no CRITICAL/HIGH remains. Checklist and severity scale:
`skills/evaluate-code/references/completion-criteria.md`.
