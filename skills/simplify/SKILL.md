---
name: simplify
description: Review the changed code for reuse, simplification, efficiency and altitude, then apply the safe fixes. Use for "/simplify", "simplify this", "clean up the diff", "tighten this before review/commit", "is there a helper for this already", and the harness's mandatory simplify gate (quality.md Verification Order step 5). Overrides the built-in /simplify. Not for bug hunting (/code-review) or whole-repo refactors.
argument-hint: "[focus on <area>] [paths…|--base <ref>]"
---

# Simplify

Review what changed, then apply only the fixes you can prove safe. Replaces the built-in `/simplify` (stock
behavior: [builtin-behavior.md](references/builtin-behavior.md)); its apply-gate is the Gate section below
(`rules/standards/quality.md` → Simplify points here). Never commit; the caller decides.

## Phase 1 - Scope

1. Change set: given paths; else `git diff $(git merge-base HEAD <base>)` plus untracked files; in a non-git
   directory, the most recently modified files.
2. Run `claude-code review simplify [paths…] [--base <ref>]` (`-h` lists `--json`, `--threshold`); its complexity
   and size findings are detector input for Phase 2, not a verdict.
3. Count changed lines; `focus on <area>` narrows Phase 2 to matching angle(s).

## Phase 2 - Review

- **Reuse**: new code duplicating an existing helper, util or stdlib call.
- **Simplification**: needless nesting, dead branches, over-abstraction and oversized functions (see thresholds below).
- **Efficiency**: avoidable O(n²), repeated work, redundant allocation or I/O.
- **Altitude**: root-cause fix, or one more special case on top of the real problem?

Under ~100 changed lines, one in-context pass over all four; otherwise four parallel read-only reviewers (one per
angle), each given the full diff and detector output. Findings: `file:line · angle · proposed edit · the assumption
that makes it safe`. No stated assumption = a flag, not a fix.

## Phase 3 - Apply

Merge duplicate findings, then apply the safe ones, gated by the Gate section below.
Edit in place and minimally: every changed line traces to a finding; no drive-by formatting or renames.
Re-run lint, scoped tests and the live check; on a regression revert your edit by hand (never
`git checkout`/`restore`/`stash` in the shared checkout) and downgrade it to a flag.

## Phase 4 - Report

Per touched function: Δ lines and Δ complexity (before → after). Then:

- **Applied**: `file:line`, what changed, the assumption.
- **Flagged**: not applied, and why (no covering test, unprovable assumption, owner decision).
- **Bugs found**: `file:line`, symptom; fixed right away (CLAUDE.md guideline 3), or a fix leaf dispatched.
- **Skipped**: reviewed and left alone on purpose (e.g. gotcha comment kept), with why.

State which gate steps ran (lint, scoped tests, live check) and which did not.

## Gate

1. **Name the assumption before each cut** ("unreachable because ___"); can't finish the sentence → flag, don't cut.
2. **Never cut** semantic or gotcha comments, distinct test scenarios, error context (`%w`, messages), or a guard
   whose impossible state you can't prove.
3. **No covering test → flag, don't cut.** The scoped tests and live check pass before AND after.
4. **Reuse first**: grep the call sequence, sibling files and `git log -S`; compare a near-duplicate with its original
   for behavior the copy lost (isolation, error handling), then consolidate at the source (say so).
5. **Altitude**: fix the root cause, not one more special case.
6. **Thresholds**: use `rules/standards/quality.md` and the findings from `claude-code review simplify`.
   Under ~100 changed lines one in-context pass replaces the fan-out.
7. **Report numbers**: Δ lines and Δ complexity per touched function, plus what was skipped and why.
