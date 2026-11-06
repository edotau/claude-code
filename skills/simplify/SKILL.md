---
name: simplify
description: Review the changed code for reuse, simplification, efficiency and altitude, then apply the safe fixes. Use for "/simplify", "simplify this", "clean up the diff", "tighten this before review/commit", "is there a helper for this already", and the harness's mandatory simplify gate (quality.md Verification Order step 5). Overrides the built-in /simplify. Not for bug hunting (/code-review) or whole-repo refactors.
argument-hint: "[focus on <area>] [paths…|--base <ref>]"
---

# Simplify

Review what changed, then apply only the fixes you can prove safe. This skill intentionally replaces the built-in
`/simplify` (notes on the stock behavior: [builtin-behavior.md](references/builtin-behavior.md)) and adds the
harness gate from `/Users/edotau/.claude/rules/standards/quality.md` → Simplify, which is the source of truth.
Never commit; the caller decides.

## Phase 1 - Scope

1. Pick the change set: given paths; else `git diff $(git merge-base HEAD <base>)` plus untracked files; in a
   non-git directory, the most recently modified files. Reviewing everything instead of the diff buries signal.
2. Run `claude-code review simplify [paths…] [--base <ref>]` (`-h` lists `--json`, `--threshold`). Its complexity and
   size findings are detector input for Phase 2, not a verdict.
3. Count changed lines. A `focus on <area>` argument narrows Phase 2 to the matching angle(s).

## Phase 2 - Review

Four angles:

- **Reuse**: new code that duplicates an existing helper, util or stdlib call.
- **Simplification**: needless nesting, dead branches, over-abstraction, functions over 50 lines.
- **Efficiency**: avoidable O(n²), repeated work, redundant allocation or I/O.
- **Altitude**: is this the root-cause fix, or one more special case on top of the real problem?

Under ~100 changed lines, do one in-context pass over all four angles; a fan-out costs more than it finds.
Otherwise launch four read-only reviewers in parallel (one per angle), each given the full diff and the detector
output. Each returns findings as `file:line · angle · proposed edit · the assumption that makes it safe`. A finding
with no stated assumption is a flag, not a fix.

## Phase 3 - Apply

Merge duplicate findings across angles, then apply the safe ones. Gate every edit with these rules (restated from
quality.md → Simplify; if they differ, quality.md wins):

1. **Name the assumption before each cut** ("unreachable because ___"). Cannot finish the sentence: flag it.
2. **Never cut** semantic or gotcha comments, distinct test scenarios, error context (`%w`, messages), or a guard
   whose impossible state you cannot prove. A nil-guard that looks redundant usually protects a path you have not
   traced; trace callers first.
3. **No covering test: flag, do not cut.** Scoped tests and a live check of the changed surface must pass before AND
   after. A cut near a test's subject re-runs `claude-code test mutate <file> <old> <new> <pkg>`.
4. **Reuse first**: grep the call sequence and sibling files, and use `git log -S`. Compare a near-duplicate with its
   original for behavior the copy lost (isolation, error handling), then consolidate at the original and say so.
5. **Altitude**: fix the root cause, not another special case. Bugs found are reported (to `/code-review`), never
   fixed here; a simplification must preserve behavior, bugs included.
6. **Thresholds**: functions under 50 lines, nesting at most 4, complexity at most 10 (`review-simplicity`).
7. **Report numbers** (Phase 4).

Edit in place and minimally: every changed line must trace to a finding. No drive-by formatting or renames.
After edits, re-run lint, the scoped tests and the live check; if anything regressed, revert your edit by hand (never
`git checkout`/`restore`/`stash` in the shared checkout) and downgrade it to a flag.

## Phase 4 - Report

Per touched function: Δ lines and Δ complexity (before → after). Then three lists:

- **Applied**: `file:line`, what changed, the assumption.
- **Flagged**: not applied, and why (no covering test, unprovable assumption, needs owner decision).
- **Bugs reported**: `file:line`, symptom, suggested `/code-review` follow-up. Not fixed.
- **Skipped**: reviewed and left alone on purpose (e.g. gotcha comment kept), with why.

State which gate steps ran (lint, scoped tests, live check, mutate) and which did not.
