# Quality Standards

Language specifics: `rules/software/<lang>/` (Go: `rules/harness/go.md`).

## Rules

1. **Immutability (CRITICAL)** — return a new copy (`update(orig, k, v)`), never mutate in place;
   language-specific idioms win.
2. **Handle every error** — user-friendly in UI code, detailed context server-side, never swallowed.
   Validate external input (user, API, file) at the boundary; fail fast.
3. **Clean breaks in internal code** — rename outright and fix every caller; no aliases or shims.
   Published APIs get a deprecation path.

## Code Quality Checklist

Functions ≤ 50 lines; files ≤ 1200 (tests ≤ 1500); imports ≤ 25; nesting ≤ 4 (use early returns).
No hardcoded values, secrets or debug prints. `rtk make lint` (`claude-code review simplify --fail`) enforces
caps on new Go lines and advises on retired words in added Go JSON tags and flags. Trim comments first.
Vault `COMPLEXITY_GATE=strict|recommended` defaults to `strict`; `recommended` makes `agents diff --check`
report high/cyclomatic complexity once per function as advisory. Other caps still fail.

Caps encourage stable reuse: write once, stress-test, reuse with minimal further tests. Never meet a cap by
adding/splitting files or thin wrappers. Find existing code, fold duplicates, extend the closest function and
remove what reuse makes dead. A new file needs a distinct role. Report file count and net lines; reduce code,
tests and test runtime where possible.

## Comments

**One line, ≤120 chars per section**, in every language, including docstrings. Exempt: provenance/audit blocks,
file-header runbooks and gotcha notes. Cut excess prose; never rewrap it. Paragraphs belong in docs.
Enforced by the same gate; cleanup list: `claude-code review quality --comments --format tsv <paths>`.

- Explain only a why, gotcha, unit or invariant the code cannot show; never narrate the next line, name or signature.
- Docstrings: one line (≤ ~80 chars) only when useful; usually none for unexported symbols. Do not list what code shows.
- Shorten verbose comments you pass to one complete sentence that keeps the point.

Performance: same work, no checks dropped, measured before/after (`standing-requirements.md` → R22).

## Simplify — the gate behind `skills/simplify`

Apply-gate (assumption before each cut, never-cut list, reuse, thresholds, numbers): `skills/simplify/SKILL.md`.

## Verification Order — lint first, always

1. **Every edit: parse/compile, lint and IDE diagnostics.** Use `ruff`, `go vet`,
   `claude-code agents diff --check --path <files>` and `mcp__ide__getDiagnostics` on each touched file.
2. **Live check:** run every changed runnable verb, endpoint or flow on real input, before any new test.
3. **Every edit:** verify the changed behavior with a targeted assertion and negative control.
4. **Scoped tests:** name touched packages; run at sign-off (`worktree land`). A leaf runs only tests it wrote/repairs.
5. **Simplify:** `Skill(simplify)` on every code change and each wave's merged diff; repeat step 4 if code changes.
6. **Full suite:** the main session alone, once (at most twice) at landing, no approval needed:
   `testing-guidelines.md` → Several worktrees. A leaf runs a chain target only when its brief names it.

`rtk make lint` and `agents diff --check` only check. A leaf checks all its files in one call, then fixes them
(or uses `claude-code agents fix --path <files>`). Accepts `--path a,b`, `--path a b` or repeated flags;
a lane commit checks against main. Never check file by file: each call reruns package tests.
`rtk make format` fixes; `rtk make audit` and `rtk make install` build the current tree. Leaves may run them.
Automatic audits run after land on the lane's rebased HEAD plus landed diff, and at session end on main.

Never silence a checker (`noqa`, `type: ignore`, `pylint: disable`, wrappers); refactor to pass. Only main may
ask the owner for a rare, disproportionate-fix exception; agents never do. Lint gates later steps; keep development
checks to touched packages. IDE diagnostics are independent: never claim clean from CLI lint alone; identify
pre-existing findings. Steps 1, 2, 5 and 6 are prose-enforced; hooks only format at Stop and advise on review.
Report steps run and skipped. Judge the command's own exit code (`set -o pipefail`), never `| tail`.
Live-first and test contracts: `testing-guidelines.md`.

## Completion

Require review (`/code-review`), green scoped checks, manual checks for user-facing changes, current docs/links and
R28's bar (severity: `skills/evaluate-code/references/completion-criteria.md`). Fix bugs, diagnostics and dead code
immediately within the named priority (R25); only owner judgments (R25, R27) wait.

**Review loop (R28):** collect finished lanes without lane reviews. One other-family review of the merged diff
lists every finding at once. Reviewers may fix findings and list edits; main/author reads their diff and fixes
all remaining findings, any severity, without asking. Repeat until no findings. Three consecutive rounds finding
a new HIGH in one area stop the loop: bring the pattern and a design recommendation to the owner.
Land with no CRITICAL/HIGH; fix any remaining findings in a follow-up on main, never park them.
A leaf ends with `UNFIXED: none` or `UNFIXED: <item>; <item>`; main fixes or decides immediately without asking
(its recommendation is the decision) and reports only the outcome. `agents ask` adds this goal to writable code
briefs; review-only tasks report findings.

**Broken tests (R29):** repair a wrong test immediately if small, else use a test-repair leaf
(`skills/subagent-workflows/SKILL.md`); send real regressions to the implementer/code-fix leaf.
Main triages collector failures: fix minor causes itself; send real defects with output to the lane/new leaf.
Leaves run only the repaired test by exact name; main then reruns every package touched by repairs/failing lanes.
Never delete or skip tests; queue repairs under heavy concurrent load.
