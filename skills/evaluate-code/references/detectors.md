# Detectors — `claude-code review` reference

Advisory: findings never fail a verb; `gate` always exits 0. Structured output is `--json` (`gate` has none). Flags:
`claude-code review <tool> -h`. Run the mechanical gate first (`rules/standards/quality.md` → Verification Order).

## Detector index

| Verb (`claude-code review …`) | Role | Usage |
|---|---|---|
| `complexity` | #2 Simplicity | `<targets...> [--threshold strict\|medium\|relaxed] [--ext py,ts,tsx,js,jsx,go]` |
| `diff` | #3 Surgical | `[--diff <range>] [--file <saved.diff>]`; default is the staged diff |
| `assumptions` | #1 Clarify | `[plan.md\|-]`; plan text only (stdin default) |
| `goals` | #4 Goal-driven | `[plan.md\|-]`; plan text only (stdin default) |
| `quality` | smells, SOLID | `<file\|dir> [-l <language>] [-o FILE]` |
| `pr` | triage | `[repo] [-b main] [--head HEAD] [-o FILE]` |
| `report` | pr + quality | `[repo] [-f text\|markdown\|json] [--pr-analysis F] [--quality-analysis F] [-o FILE]` |
| `simplify` | complexity + quality on changed files | `[paths...] [--base REF] [--threshold …]` |
| `gate` | staged-change summary | `[--repo DIR]` |

## What each reports

- **complexity** — per-function cyclomatic complexity, length, nesting; per-file import count, class density,
  premature ABC/Protocol, length. `PASS`/`WARN`/`FAIL`, 0–100 per file. Thresholds: `strict` (new code), `medium`
  (default), `relaxed` (legacy).
- **diff** — whitespace-only, comment-only, docstring-addition and quote-style-swap lines; a noise ratio:
  `CLEAN` (<10%), `NOISY` (<30%), `VERY_NOISY`.
- **assumptions** — "just", "obviously", "should work", absolute scope ("all users"), vague verbs, unscoped
  subjects, numbered plans with no verification. `CLEAN`/`REVIEW`/`CLARIFY`; heuristic, expect false positives.
- **goals** — each plan step 0–3: `3` runnable check (assert/pytest/exit 0/curl/grep), `2` manual, `1` vague, `0`
  none; also a final end-to-end step. `STRONG` (≥70%), `WEAK` (≥40%), `MISSING`.
- **quality** — long functions, many parameters, god classes, magic numbers, commented-out code, SOLID signals,
  0–100. Regex-based (breadth over precision); prefer `complexity` for per-function metrics.
- **pr** — files by path risk (auth/security → critical), risky added lines (secrets, SQL concatenation, debugger,
  `any`, eslint-disable), conventional-commit lint, 1–10 complexity score, review order. Needs a git repo.
- **report** — verdict (`approve` … `block`), 0–100 score, P0–P2 actions, findings by severity.

`assumptions` and `goals` feed the `Plan` agent's self-review (`agents/Plan.md`); never point them at source files.

```bash
claude-code review complexity src/auth/ --threshold strict --json
claude-code review diff --diff HEAD~3..HEAD
echo "I'll just export all user data" | claude-code review assumptions -
```

## Manual review cues (no detector flags these)

- **Off-topic mass deletion or clobber** — a large deletion/rewrite in a file the commit subject isn't about (docs
  replaced inside a `fix: cicd` commit) is an accidental overwrite until `git diff --stat` says otherwise. Recover
  with `git show <commit>~1:<path> > <path>`.
- **Unguarded duplicate of a guarded side effect** — a new top-of-module call (`add_safe_globals`,
  `filterwarnings`, monkeypatch, env swap): grep for an existing guarded version first.
- **Declared-but-unwired config variable** — in DAB/Terraform, a variable passed but never referenced (`${var.x}`)
  is dead code `validate` can't see; grep declared against referenced.

## Enforcement levels

The gate is opt-in and not in the seeded `settings.json`; higher levels catch more at more friction.

| Level | Mechanism | Setup |
|---|---|---|
| 1 Passive | the skill loads as context | none |
| 2 Active | `/code-review` before commit: diff scope, detectors, quality lenses, verdict | a command (default) |
| 3 Gate | `claude-code review gate` on staged files; awareness only, never blocks | git pre-commit or a PreToolUse hook |
| 4 CI | detectors as PR checks; fail the job yourself, since the verbs exit 0 | CI config |

- **Level 3, git:** `.git/hooks/pre-commit` = `#!/bin/sh` + `exec claude-code review gate`, then `chmod +x`.
- **Level 3, PreToolUse:** declare it in `internal/settings/hookspec.go` (matcher `Bash`, command
  `claude-code review gate`); the owner runs `claude-code configure --restore` (or edits `settings.json`).
- **Level 4:** parse the JSON verdict, e.g.
  `claude-code review complexity $(git diff --name-only origin/main...HEAD) --json | jq -e '.verdict != "FAIL"'`
  (exits 1 when no file matches: skip on a diff with no source files) and
  `claude-code review diff --diff origin/main...HEAD --json`. Fail on `FAIL` only or `WARN` too per team
  tolerance; a regulated repo also fails on CRITICAL/HIGH from `/security-review`.
