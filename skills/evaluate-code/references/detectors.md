# Detectors — full CLI reference

All detectors are `claude-code review <sub>` verbs — same flags as the scripts they replaced.
All are **advisory** and **exit 0** (never a hard gate) except where noted. All structured
output is `--json`.

---

## Lint & format gate (run before detectors)

Before the discipline detectors, run the project's mechanical format + lint gate —
deterministic, catches trivial issues no human should review, and a noisy diff from
unformatted code masks the real change. Unlike the detectors below, **this gate can fail**:
treat a non-zero exit as a must-fix before review proceeds.

```bash
# Prefer the project's own gate; fall back to the tools directly.
if make -n lint >/dev/null 2>&1; then
  make fmt   && make lint            # the project's own formatter + linter
else
  black --check . && ruff check .    # generic Python fallback
fi
```

`make fmt` auto-applies formatting; `make lint` is check-only (see the project's `Makefile`). In a
Go repo that is `gofmt -l` + `go vet`; in a JS/TS repo substitute the
project's equivalent (`npm run lint`, `prettier --check`). Report formatting/lint failures as
**should-fix** (or **nit** once auto-fixed) — they precede, not replace, the judgment passes.

---

## `claude-code review complexity` — Principle #2 (Simplicity First)

Detects over-engineering. Python is analyzed via an indentation/keyword estimate (not an
AST); TS/JS use the same heuristics as before. Go files are not analyzed.

```
claude-code review complexity <targets...> [--threshold strict|medium|relaxed]
                                            [--ext py,ts,tsx,js,jsx] [--json]
```

Checks: per-function cyclomatic complexity, function length, nesting depth; per-file import
count, class density, premature ABC/Protocol in small files, file length.

Thresholds: `strict` (new code) · `medium` (default) · `relaxed` (legacy). Verdict:
`PASS` / `WARN` / `FAIL`. Each file gets a 0–100 score.

```bash
claude-code review complexity src/auth/ --threshold strict --json
```

---

## `claude-code review diff` — Principle #3 (Surgical Changes)

Flags changed lines that don't trace to the stated goal.

```
claude-code review diff [--diff <range>] [--file <saved.diff>] [--json]
```

Default reads `git diff --cached`. Flags: whitespace-only, comment-only, docstring
additions, quote-style swaps. Reports a **noise ratio** → `CLEAN` (<10%) / `NOISY` (<30%) /
`VERY_NOISY`. Usage errors exit 2; a missing `--file` exits 1; findings themselves stay exit 0.

```bash
claude-code review diff                       # staged
claude-code review diff --diff HEAD~3..HEAD
```

### Manual review cues (things the detector can't flag)

Two recurring "change doesn't trace to the goal" failure modes that no automated detector
catches — check them by hand on any diff:

- **Off-topic mass deletion / clobber.** A large deletion or wholesale rewrite in a file
  whose commit subject is about something else (docs replaced inside a `fix: cicd`
  commit; a config file gutted in a feature commit) is a red flag for an *accidental
  overwrite*, often by boilerplate from an upstream tool. When `git diff --stat` shows a
  big deletion the commit message doesn't mention, diff the actual content and confirm
  intent before accepting it. Recover with `git show <commit>~1:<path> > <path>` if
  clobbered.
- **Duplicate that reintroduces the bug a guard prevented.** When a diff adds a
  top-of-module side-effecting call (`torch.serialization.add_safe_globals`,
  `warnings.filterwarnings`, a monkeypatch, an env swap), grep the same module for an
  existing *guarded* version first. A common hasty-patch failure is adding an *unguarded*
  duplicate above a properly guarded one — it both duplicates the effect and re-opens the
  crash/edge-case the guard was defending against.
- **Declared-but-unwired config variables.** In declarative infra (DAB, Terraform, any
  config DSL), a variable that's declared and passed but never `${var.x}`-referenced is
  the config analogue of dead code. It escapes `validate` gates because defaults satisfy
  them — only a "declared vs referenced" grep catches it.

---

## `claude-code review assumptions` — Principle #1 (Think Before Coding)

Reads a markdown plan (or stdin) and flags hidden assumptions.

```
claude-code review assumptions [plan.md|-] [--json]
```

Flags: minimizing language ("just", "simply"), unstated assumptions ("obviously"), hopeful
phrasing ("should work"), absolute scope ("all users", "always"), vague action verbs,
unscoped subjects ("the user"), numbered plan blocks with no verification step. Verdict:
`CLEAN` / `REVIEW` / `CLARIFY`. Heuristic — false positives expected; the point is to start
a conversation about assumptions before coding.

```bash
echo "I'll just export all user data" | claude-code review assumptions -
```

**Consumer: the `planning` skill** — its Self-Review Checklist pipes drafted plans through
this detector and `review goals`. Both analyze **plan text via stdin (`-`) or a plan .md**,
never code files — don't point them at source files.

---

## `claude-code review goals` — Principle #4 (Goal-Driven Execution)

Scores each plan step 0–3 on verification quality.

```
claude-code review goals [plan.md|-] [--json]
```

Scoring: `3` concrete runnable check (assert/pytest/exit 0/status 200/curl/grep) · `2`
manual check (verify/confirm/inspect) · `1` vague ("should work") · `0` none. Also checks
for a final/end-to-end step. Verdict: `STRONG` (≥70%) / `WEAK` (≥40%) / `MISSING`.

```bash
claude-code review goals implementation-plan.md --json
```

---

## `claude-code review quality` — quality (smells + SOLID)

Multi-language smells, SOLID violations, and a 0–100 quality score.

```
claude-code review quality <file|dir> [--recursive] [--language python|typescript|javascript|go|swift|kotlin]
                                       [--json] [--output FILE]
```

Smells: long functions, too many parameters, high complexity, god classes, magic numbers,
commented-out code. SOLID: OCP (type-checking), LSP/ISP (NotImplementedError), DIP
(import-heavy). **Exit code 1** if the target path does not exist (else 0).

> For precise per-function Python metrics prefer `review complexity`. `review quality` is
> regex-based across 6 languages — better breadth, lower per-function precision.

---

## `claude-code review pr` — triage

Risk-categorizes a PR's changed files and lints commit messages. **Requires a git repo**
(exit 1 if not).

```
claude-code review pr [repo_path] [--base main] [--head HEAD] [--json] [--output FILE]
```

Categorizes files critical/high/medium/low by path (auth/security → critical), scans added
lines for risky patterns (hardcoded secrets, SQL concatenation, debugger, console.log, `any`
type, eslint-disable), validates conventional-commit format, and emits a 1–10 complexity
score with a suggested review order.

---

## `claude-code review report` — orchestration

Combines `review pr` + `review quality` into one report.

```
claude-code review report [repo_path] [--format text|markdown|json]
                           [--pr-analysis pr.json] [--quality-analysis quality.json]
                           [--output FILE]
```

Produces a verdict (`approve` / `approve_with_suggestions` / `request_changes` / `block`), a
0–100 score, prioritized P0–P2 action items, and findings grouped by severity. **Exit 1**
if the path does not exist.
