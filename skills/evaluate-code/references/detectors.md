# Detectors — full CLI reference

All scripts live in `skills/evaluate-code/scripts/` — **except `complexity_checker.py` (#2),
which is canonical in `skills/simplicity/scripts/`** (resolved via `$SIMP` in its section
below). All are **stdlib-only** (no venv) and **exit 0** (advisory signals, never a hard
gate) except where noted. All structured output is `--json`.

Resolve the dirs from the Claude config dir:

```bash
ROOT="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
CR="$ROOT/skills/evaluate-code/scripts"
SIMP="$ROOT/skills/simplicity/scripts"   # complexity_checker.py
```

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

## complexity_checker.py — Principle #2 (Simplicity First)

> **Canonical home: the `simplicity` skill** (`skills/simplicity/scripts/`), not
> `evaluate-code/scripts/`. `simplicity` owns the #2 apply-the-fix material; the path below
> uses `$SIMP`, resolved to that skill's `scripts/` dir.

Detects over-engineering. Python is analyzed with `ast` (accurate per-function metrics);
TS/JS fall back to regex/indentation heuristics.

```
complexity_checker.py <file|dir> [--threshold strict|medium|relaxed]
                                 [--ext py,ts,tsx,js,jsx] [--json]
```

Checks: per-function cyclomatic complexity, function length, nesting depth; per-file import
count, class density, premature ABC/Protocol in small files, file length.

Thresholds: `strict` (new code) · `medium` (default) · `relaxed` (legacy). Verdict:
`PASS` / `WARN` / `FAIL`. Each file gets a 0–100 score.

```bash
# $SIMP resolved by the 4-step loop at the top of this file.
python3 "$SIMP/complexity_checker.py" src/auth/ --threshold strict --json
```

---

## diff_surgeon.py — Principle #3 (Surgical Changes)

Flags changed lines that don't trace to the stated goal.

```
diff_surgeon.py [--diff <range>] [--file <saved.diff>] [--json]
```

Default reads `git diff --cached`. Flags: whitespace-only, comment-only, docstring
additions, quote-style swaps. Reports a **noise ratio** → `CLEAN` (<10%) / `NOISY` (<30%) /
`VERY_NOISY`.

```bash
python3 "$CR/diff_surgeon.py"                    # staged
python3 "$CR/diff_surgeon.py" --diff HEAD~3..HEAD
```

### Manual review cues (things the script can't flag)

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

## assumption_linter.py — Principle #1 (Think Before Coding)

Reads a markdown plan (or stdin) and flags hidden assumptions.

```
assumption_linter.py <plan.md|-> [--json]
```

Flags: minimizing language ("just", "simply"), unstated assumptions ("obviously"), hopeful
phrasing ("should work"), absolute scope ("all users", "always"), vague action verbs,
unscoped subjects ("the user"), numbered plan blocks with no verification step. Verdict:
`CLEAN` / `REVIEW` / `CLARIFY`. Heuristic — false positives expected; the point is to start
a conversation about assumptions before coding.

```bash
echo "I'll just export all user data" | python3 "$CR/assumption_linter.py" -
```

**Consumer: the `planning` skill** — its Self-Review Checklist pipes drafted plans through
this linter and `goal_verifier.py`. Both analyze **plan text via stdin (`-`) or a plan .md**,
never code files — don't point them at source files.

---

## goal_verifier.py — Principle #4 (Goal-Driven Execution)

Scores each plan step 0–3 on verification quality.

```
goal_verifier.py <plan.md|-> [--json]
```

Scoring: `3` concrete runnable check (assert/pytest/exit 0/status 200/curl/grep) · `2`
manual check (verify/confirm/inspect) · `1` vague ("should work") · `0` none. Also checks
for a final/end-to-end step. Verdict: `STRONG` (≥70%) / `WEAK` (≥40%) / `MISSING`.

```bash
python3 "$CR/goal_verifier.py" implementation-plan.md --json
```

---

## code_quality_checker.py — quality (smells + SOLID)

Multi-language smells, SOLID violations, and a 0–100 quality score. Thin CLI over
`quality_core.py`.

```
code_quality_checker.py <file|dir> [--recursive] [--language python|typescript|javascript|go|swift|kotlin]
                                    [--json] [--output FILE]
```

Smells: long functions, too many parameters, high complexity, god classes, magic numbers,
commented-out code. SOLID: OCP (type-checking), LSP/ISP (NotImplementedError), DIP
(import-heavy). **Exit code 1** if the target path does not exist (else 0).

> For precise per-function Python metrics prefer `complexity_checker.py` (AST). This script
> is regex-based across 6 languages — better breadth, lower per-function precision.

---

## pr_analyzer.py — triage

Risk-categorizes a PR's changed files and lints commit messages. **Requires a git repo**
(exit 1 if not).

```
pr_analyzer.py [repo_path] [--base main] [--head HEAD] [--json] [--output FILE]
```

Categorizes files critical/high/medium/low by path (auth/security → critical), scans added
lines for risky patterns (hardcoded secrets, SQL concatenation, debugger, console.log, `any`
type, eslint-disable), validates conventional-commit format, and emits a 1–10 complexity
score with a suggested review order.

---

## review_report_generator.py — orchestration

Combines `pr_analyzer.py` + `code_quality_checker.py` into one report. Resolves both
sibling scripts via `Path(__file__).parent`, so it works wherever the bundle lives.

```
review_report_generator.py [repo_path] [--format text|markdown|json]
                           [--pr-analysis pr.json] [--quality-analysis quality.json]
                           [--output FILE]
```

Produces a verdict (`approve` / `approve_with_suggestions` / `request_changes` / `block`), a
0–100 score, prioritized P0–P2 action items, and findings grouped by severity. **Exit 1**
if the path does not exist.
