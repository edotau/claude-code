---
name: evaluate-code
description: 'Investigating and auditing existing work (harness variant of the built-in /code-review): review a diff or
PR, audit a transcript against the evidence loop, judge a claimed-done (VERIFIED/REFUTED), scan tech debt, review a
skill or plugin; discipline, performance, cross-boundary and contract lenses plus `claude-code review` detectors.
Triggers "review my code", "review this PR", "review my diff", "audit this", "judge this", "is that claim actually
verified", "vet this plan", "run the trap-suite eval", "check complexity", "is this slow", "N+1", "tech debt".'

metadata:
  version: 4.0.0
---

# Evaluate Code

Quality review plus the discipline lens for LLM-code failure modes. Read-only: findings and a verdict, never edits.

## Pick your path

- **Review a diff/PR** → [Review a diff](#review-a-diff).
- **Audit a transcript** → [Audit a transcript](#audit-a-transcript).
- **Judge a claimed-done** → `references/judge.md` (is the claim true, not is the code good).
- **Scan tech debt** → [Tech debt](#tech-debt).
- **Review a skill or plugin** → `references/codex-skill-plugin-review.md` (run by `agents/codex.md`).
- **A/B-eval a skill rule change** → `references/eval.md` (trap suite; `workflows/code-evaluator.js`).
- **Vet a task brief or plan** → `references/task-review.md`.

## Audit a transcript

Grade the work against its brief's `Method:` per step: **followed**, **skipped**, or **faked** (claimed, not
observed).

1. Run the detectors first — [Detectors](#detectors).
2. For every skip or fake, name the concrete risk via `references/failure-modes.md`.
3. Score on the 0–2 rubric in `references/judge.md`.
4. Deliver a short table plus the one highest-value fix; apply only if asked.

## Tech debt

Scope = a module path or the repo.

```bash
git diff --name-only HEAD~10 -- <scope> 2>/dev/null || git ls-files <scope> | head -50
git grep -n -E 'TODO|FIXME|HACK|XXX|NOQA' -- <scope> | head -30
```

1. Read the files with the most markers.
2. Score each item 1–5 on Impact, Risk, Effort; `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
3. Suggest a fix for the top 3; output a table, highest first:
   `| Score | Category | File(s) | Description | Impact | Risk | Effort |`.
4. To fix: apply the top item, re-run the `git grep`, then the next; stop if a fix needs a design decision.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.

## Lenses

Report only findings on changed code at >80% confidence; consolidate similar ones; skip style unless CRITICAL.

### Discipline — the four principles

Defined in `rules/standards/principles.md`; apply to changes >20 lines or unfamiliar code. Detectors:
`references/detectors.md`; examples: `references/anti-patterns.md`.

| Principle | As a review finding | Severity |
|---|---|---|
| **1 Clarify Assumptions** | silently picked one reading of an ambiguous requirement — a **question**, not a decision | should-fix |
| **2 Simplicity First** | would a senior engineer call it overcomplicated? Only **flag**; the fix is the simplify pass's — send it back | should-fix |
| **3 Surgical Changes** | drive-by reformatting, comment churn, refactors of untouched code | nit — **should-fix** if they hide the real change |
| **4 Goal-Driven Execution** | success criteria not concrete, or not actually verified | should-fix |

### Performance

Query, data loop, hot path, or a speed/memory review: **measure before you optimize**; no number = hypothesis.
Tables and report format: `references/performance.md`.

### Cross-boundary

A seam (function signature, argv/env builder, flag, file format, API another package calls): read the producer and
every consumer together and exercise each consumer path; verified only on every caller.

### PR / API contract

A PR over ~200 lines, or touching a shared module, API serializer/schema, migration or shared table: run
**blast-radius** and **breaking-change** detection per `references/pr-and-api-review.md`.

## Detectors

`claude-code review <sub>` verbs: advisory, never a hard gate. Index, flags, enforcement levels:
`references/detectors.md`.

## Review a diff

This skill is what the built-in `/code-review`'s quality reviewer loads.

```bash
BASE_SHA=$(git merge-base HEAD main); git diff ${BASE_SHA}...HEAD --stat
```

1. Mechanical gate first: `rules/standards/quality.md` → Verification Order.
2. Run the detectors on the changed files, unless their output is in your brief.
3. Apply the lenses to the diff and changed files, not session history; verify each finding in the code.
4. Checklist: `rules/standards/quality.md` → Code Quality Checklist; severity:
   `references/completion-criteria.md`; the call: [Verdicts](#verdicts).

## Verdicts

The verdict word comes first, alone, from a closed vocabulary; evidence follows. Claims that can't be re-run are
UNVERIFIABLE. Never soften a failing verdict or inflate a caveat.

| Mode | Vocabulary |
|---|---|
| review (this file; `claude-code <agent> review`) | **SHIP** (no CRITICAL/HIGH; MEDIUM fixed first) \| **FIX REQUIRED** (HIGH, no CRITICAL) \| **BLOCK** (any CRITICAL) |
| judge (`references/judge.md`) | `VERIFIED \| VERIFIED WITH CAVEATS \| REFUTED` |
| task-brief / plan review (`references/task-review.md`) | `READY \| NEEDS CLARIFICATION` |
