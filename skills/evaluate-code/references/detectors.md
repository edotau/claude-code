# Detectors — `claude-code review` reference

Run `rules/standards/quality.md` → Verification Order first. Detectors are advisory;
`simplify --fail` gates new comment/size violations, while `gate` never blocks.
Help: `claude-code review <tool> -h`. JSON: `--json`, except `scope --format json`, `report -f json`; none for `gate`.

## Detector index

Verbs below follow `claude-code review`, except the two `plan` commands:

- **complexity:** `<targets...> [--threshold strict|medium|relaxed] [--ext py,ts,tsx,js,jsx,go]`.
- **diff:** `[--diff <range>] [--file <saved.diff>]`; staged diff by default.
- **claude-code plan assumptions:** `[plan.md|-]`; plan text only, stdin default.
- **claude-code plan goals:** `[plan.md|-]`; plan text only, stdin default.
- **quality:** `<file|dir> [-l <language>] [--json [-o FILE]] [--comments --format tsv]`.
- **pr:** `[repo] [-b main] [--head HEAD] [-o FILE]`.
- **report:** `[repo] [-f text|markdown|json] [--pr-analysis F] [--quality-analysis F] [-o FILE]`.
- **simplify:** `[paths...] [--base REF] [--threshold …] [--fail]`; `--fail` rejects `--threshold` and `--json`.
- **bench:** `[--bench RE] [--base] [--before F --after F]`; changed packages' benchmarks (R22).
- **gate:** `[--repo DIR]`; staged-change summary.
- **scope:** `(--range R | --paths p1,p2) [--caps] [--importers] [--format text|json] [--repo DIR]`.

## What each reports

- **complexity:** function complexity/length/nesting; file imports/classes, premature ABC/Protocol, length.
  Thresholds: strict for new code, medium default, relaxed for legacy.
- **diff:** whitespace/comment-only, docstring-addition, quote-style-swap and noise ratio.
- **assumptions:** “just”, “obviously”, “should work”, absolute scope, vague verbs, unscoped subjects,
  numbered plans without verification. Heuristic: expect false positives.
- **goals:** runnable/manual/vague/missing checks per step and a final end-to-end check.
- **quality:** long functions, many parameters, god classes, magic numbers, commented-out code, SOLID signals.
  Regex-based; use complexity for function metrics.
- **pr:** path risk, risky additions (secrets, SQL concatenation, debugger, `any`, eslint-disable),
  Conventional Commit lint, complexity and review order. Requires a git repo.
- **report:** combined pr/quality verdict, score, prioritized actions and severity.

`assumptions`/`goals` feed `agents/Plan.md`; never run them on source files.

## Manual review cues (no detector flags these)

- **Off-topic deletion/clobber:** treat large unrelated rewrites as accidental until the diff stat supports them;
  recover previous content from git when confirmed.
- **Unguarded duplicate side effect:** before a module-level registration, warning filter, monkeypatch or env swap,
  search for an existing guarded version.
- **Declared-but-unwired config:** compare DAB/Terraform declarations with references (`${var.x}`);
  validation misses unused passed variables.

## Enforcement levels

The gate is opt-in, absent from seeded `settings.json`:

1. **Passive:** skill context; no setup.
2. **Active (default):** `/code-review` before commit: diff, detectors, lenses, verdict.
3. **Gate:** `claude-code review gate` on staged files, advisory only; git pre-commit or PreToolUse hook.
4. **CI:** PR checks parse verdicts and fail the job explicitly; advisory verbs do not fail it for findings.

Git setup: executable `.git/hooks/pre-commit` with `#!/bin/sh` and `exec claude-code review gate`.
PreToolUse setup: `rules/harness/change-protocol.md`; register in `internal/settings/hookspec.go` and handle in
`internal/hooks/dispatch.go`; owner runs `claude-code configure restore` or edits settings.
CI: use `complexity --json` on changed supported source files (skip an empty list) and
`diff --diff origin/main...HEAD --json`; reject FAIL, optionally WARN per team tolerance.
Regulated repos also reject CRITICAL/HIGH from `/security-review`.
