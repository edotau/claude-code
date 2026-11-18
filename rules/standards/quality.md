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

## Simplify — the gate behind `skills/simplify`

The apply-gate (assumption before each cut, never-cut list, reuse first, thresholds, numbers) lives in
`skills/simplify/SKILL.md`.

## Verification Order — lint first, always

| # | Step | When |
| --- | --- | --- |
| 1 | Parse/compile + **lint** (`ruff`, `go vet`, `claude-code agents diff --check --path <files>`) **and the IDE's diagnostics** (`mcp__ide__getDiagnostics` on each file touched) | Every edit, unprompted |
| 2 | **Live check** — run the verb, endpoint or flow on real input | Every change with a runnable surface, before any new test |
| 3 | The one thing changed — targeted assertion, negative control | Every edit |
| 4 | Scoped tests for the packages you touched, named explicitly | Once the direction settles, or before commit |
| 5 | **Simplify** — `Skill(simplify)` over the (merged) diff; re-run step 4 if it changed code | Every code change, before review, merge or commit |
| 6 | Full suite + gate chain (format → lint → test → security) | **Once** at wrap-up, **only with the user's approval**, never a subagent |

`make lint` and `agents diff --check` only check (a leaf runs `claude-code agents diff --check --path <files>` and
fixes its own files from the output, or with `claude-code agents fix --path <files>`); `make fmt` fixes, `make audit`
and `make install` ship: the main session's, from a clean tree.

Lint gates the rest: a syntax error makes test output meaningless. Untouched packages are noise during development.
The IDE's diagnostics are a second gate, not a duplicate: the editor runs analyzers the repo's CLI lint does not
(Ruff rules, Pylance types, Pylint hints), so a file that is flake8/black clean can still show errors the user sees
— never call a file clean on CLI lint alone, and say which findings are pre-existing rather than silently fixing them.
Steps 1, 2, 5 and 6 are prose-enforced; hooks only format at Stop and give advisory review. Report which steps ran
and which did not. Judge a run by its own exit code (`set -o pipefail`), never `| tail`. Live first, test scope and
what makes a test a guard: `testing-guidelines.md`.

## Completion

Review done (`/code-review`), applicable scoped checks green, manual check for user-facing changes, docs and links
current; approve when no CRITICAL/HIGH remains (severity scale:
`skills/evaluate-code/references/completion-criteria.md`). Every finding, LOW included, is fixed in the same round;
only owner-judgment items go to a report. A bug noticed mid-task is fixed right away without asking.
