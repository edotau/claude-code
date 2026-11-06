---
name: evaluate-code
description: 'Investigating and auditing existing work (harness variant of the built-in /code-review): review a diff or PR, audit a transcript against the evidence loop, judge a claimed-done (VERIFIED/REFUTED), scan tech debt, review a skill or plugin; discipline, security (OWASP, secrets, injection, auth, Claude Code config, dependency CVEs and licences), performance, cross-boundary and contract lenses plus `claude-code review` detectors. Triggers "review my code", "review this PR", "review my diff", "audit this", "judge this", "is that claim actually verified", "vet this plan", "run the trap-suite eval", "check complexity", "is this slow", "N+1", "tech debt", "security review", "is this safe", "check for secrets", "OWASP", "audit dependencies", "any CVEs in our deps", "license compliance".'

metadata:
  version: 4.0.0
---

# Evaluate Code

**Investigating and auditing existing work** — code **quality** review plus the discipline lens
that catches how LLM-written code characteristically fails. Read-only: findings and a verdict, never edits.

## Pick your path

- **Review a diff/PR** → [Review a diff](#review-a-diff).
- **Security review (code, Claude Code config, dependencies)** → [Security](#security) lens.
- **Audit a transcript** → [Audit a transcript](#audit-a-transcript).
- **Judge a claimed-done** → `references/judge.md` (is the claim true, not whether the code is good).
- **Scan tech debt** → [Tech debt](#tech-debt).
- **Review a skill or plugin** → `references/codex-skill-plugin-review.md` (run by `agents/codex.md`).
- **A/B-eval a skill rule change** → `references/eval.md` (trap suite; `workflows/code-evaluator.js`).
- **Vet a task brief or plan before implementation** → `references/task-review.md`.

## Audit a transcript

Grade the most recent completed work against the method its brief named (`Method:`), step by step: each
step marked **followed**, **skipped**, or **faked** (claimed without observation).

1. Run the detectors first (mechanical pass) — [Detectors](#detectors).
2. For every skip or fake, name the concrete risk it created, via `references/failure-modes.md`.
3. Score the work on the 0–2 rubric in `references/judge.md`.
4. Deliver a short table plus the single highest-value fix. Apply it only if asked.

## Tech debt

Scope = a module path or the repo.

```bash
git diff --name-only HEAD~10 -- <scope> 2>/dev/null || git ls-files <scope> | head -50
git grep -n -E 'TODO|FIXME|HACK|XXX|NOQA' -- <scope> | head -30
```

1. Read the files with the highest marker concentration.
2. Score each item 1–5 on Impact, Risk and Effort; rank by `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
3. Suggest a concrete fix for the top 3. Output a table, highest first:
   `| Score | Category | File(s) | Description | Impact | Risk | Effort |`.
4. To fix: take the top item, apply it, re-run the `git grep`, then the next; stop if a fix needs a design decision.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.

## Lenses

Report only findings on changed code at >80% confidence; consolidate similar findings; skip stylistic preferences
unless CRITICAL.

### Discipline — the four principles

`rules/standards/principles.md` defines them; here is how each shows up as a finding. Apply to any non-trivial
change (>20 lines), unfamiliar code, or anything a human will review. Detector per principle:
`references/detectors.md`; worked examples: `references/anti-patterns.md`.

| Principle | As a review finding | Severity |
|---|---|---|
| **1 Clarify Assumptions** | silently picked one reading of an ambiguous requirement — a **question**, not a decision | should-fix |
| **2 Simplicity First** | *would a senior engineer call this overcomplicated?* Only **flag**; the fix is the simplify pass's, which ran before this review — send it back there | should-fix |
| **3 Surgical Changes** | drive-by reformatting, comment churn, refactors of untouched code | nit — **should-fix** if they hide the real change |
| **4 Goal-Driven Execution** | success criteria not concrete, or not actually verified | should-fix |

### Performance

A change touching a query, a loop over data or a hot path, or a review *about* speed/memory: **measure before you
optimize** and rank findings by measured impact (no number = hypothesis). The usual target is the N+1 query.
Symptom→fix tables and the report format: `references/performance.md`.

### Cross-boundary

A change touching a seam (function signature, argv/env builder, flag, file format, an API another package calls):
read the producer and every consumer together and exercise each consumer path; a seam is verified only on every
caller.

### PR / API contract

A PR/diff over ~200 lines, or one touching a shared module, an API serializer/schema, a DB migration or a table
other jobs read: run **blast-radius** and **breaking-change** detection (adding is usually safe; removing,
renaming, retyping or tightening usually isn't). Commands, the safe-vs-breaking table and the two-phase migration
rule: `references/pr-and-api-review.md`.

## Detectors

`claude-code review <sub>` verbs: advisory, never a hard gate. Verb index, flags and enforcement levels (the opt-in
`gate`, CI): `references/detectors.md`.

## Review a diff

This skill is what the built-in `/code-review`'s quality reviewer loads.

```bash
BASE_SHA=$(git merge-base HEAD main); git diff ${BASE_SHA}...HEAD --stat
```

1. Mechanical gate first: `rules/standards/quality.md` → Verification Order.
2. Run the detectors on the changed files, unless their output is already in your brief.
3. Apply the lenses to the diff range + changed files, not the session history; verify every finding by reading
   the code.
4. Pre-approval checklist: `rules/standards/quality.md` → Code Quality Checklist; severity:
   `references/completion-criteria.md`; the call: [Verdicts](#verdicts).
5. Security-sensitive code (see [Security](#security)) gets that lens as well.

## Verdicts

The verdict word comes first, alone, from a closed vocabulary; evidence follows. Claims that can't be re-run are
UNVERIFIABLE, never assumed. Never soften a failing verdict or inflate a caveat.

| Mode | Vocabulary |
|---|---|
| review (this file) | **SHIP** (no CRITICAL/HIGH) \| **FIX REQUIRED** (HIGH, no CRITICAL) \| **BLOCK** (any CRITICAL) |
| judge (`references/judge.md`) | `VERIFIED \| VERIFIED WITH CAVEATS \| REFUTED` |
| task-brief / plan review (`references/task-review.md`) | `READY \| NEEDS CLARIFICATION` |
| agent diff review (`claude-code <agent> review`) | `SHIP IT \| NEEDS WORK \| BLOCK` |
