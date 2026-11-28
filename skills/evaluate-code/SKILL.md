---
name: evaluate-code
description: >-
  Audit existing work (/code-review variant): diff/PR review, transcript audit, claimed-done judge
  (VERIFIED/REFUTED), tech debt, skill review and `claude-code review` detectors. Use for "review my
  code/PR/diff/skill", "audit/judge this", "vet this plan", "check complexity", "tech debt".
metadata:
  version: 4.0.0
---

# Evaluate Code

Review existing work: findings and a verdict, read-only unless the collector review permits fixes
(`rules/standards/quality.md` → Completion). Judge and task-review modes remain read-only.

## Pick your path

- Diff/PR → **Review a diff**; transcript → **Audit a transcript**; debt → **Tech debt**.
- Claimed-done → `references/judge.md`; task brief/plan → `references/task-review.md`.
- Skill/plugin → `references/codex-skill-plugin-review.md` (carried `codex` ask).
- A/B skill-rule eval → `references/eval.md`.

## Audit a transcript

Grade each step against the brief's `Method:`: **followed**, **skipped**, **faked** (claimed without observation).
Run **Detectors** first; name each skip/fake's risk from `references/failure-modes.md`, score with
`references/judge.md`, then give a short table and the highest-value fix. Apply only if asked.

## Tech debt

Scope a module or repo. Inspect files changed in the last ten commits (fallback: tracked files) and search
`TODO|FIXME|HACK|XXX|NOQA` with `git grep -n -E`; read files with the most markers.
Score each item 1–5 on Impact, Risk, Effort: `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
Give the top three fixes, highest first: `| Score | Category | File(s) | Description | Impact | Risk | Effort |`.
When asked to fix, apply the top item, rerun the search, continue; stop at a design decision.
Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.

## Lenses

Report changed-code findings at >80% confidence. Verify in code, consolidate similar findings and list all at once,
including style (R28).

### Discipline — the four principles

For >20 changed lines or unfamiliar code, apply `rules/standards/principles.md`:
**1 Clarify** (silent interpretation of ambiguity: ask, don't decide), **2 Simplicity** (flag; simplify pass fixes),
**4 Goal-Driven** (criteria concrete and verified) are should-fix. **3 Surgical** (unrelated formatting, comments,
refactors) is a nit, should-fix when it hides the change. Detectors: `references/detectors.md`;
examples: `references/anti-patterns.md`.

### Performance

For queries, data loops, hot paths or speed/memory reviews, measure first; no number means hypothesis.
Use `references/performance.md`.

### Cross-boundary

For signatures, argv/env builders, flags, formats and shared APIs, read producer and every consumer together.
Exercise every consumer path before calling the seam verified.

### PR / API contract

Over ~200 lines, or touching shared modules, serializers/schemas, migrations or shared tables:
run both passes in `references/pr-and-api-review.md`.

## Detectors

`claude-code review <sub>` is advisory by default; `simplify --fail` gates `rtk make lint`.
Verbs, flags and enforcement: `references/detectors.md`.

## Review a diff

The built-in `/code-review` quality reviewer loads this skill.

1. Establish diff scope: `rtk git diff "$(git merge-base HEAD main)"...HEAD --stat`.
2. Mechanical gate: `rules/standards/quality.md` → Verification Order.
3. Run detectors on changed files unless the brief already carries their output.
4. Apply lenses to diff/files, not session history. Checklist: `rules/standards/quality.md` → Code Quality Checklist;
   severity: `references/completion-criteria.md`; verdict below.

## Verdicts

Put the verdict alone on the first line, evidence after. An unreproducible claim is **UNVERIFIABLE** (claim status,
not verdict). Never soften failure or inflate a caveat.

- **Review:** **SHIP** = no CRITICAL/HIGH, MEDIUM fixed first; **FIX REQUIRED** = HIGH without CRITICAL;
  **BLOCK** = any CRITICAL (`claude-code <agent> review` uses these too).
- **Judge:** `VERIFIED | VERIFIED WITH CAVEATS | REFUTED` (`references/judge.md`).
- **Task brief / plan:** `READY | NEEDS CLARIFICATION` (`references/task-review.md`).
