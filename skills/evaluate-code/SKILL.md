---
name: evaluate-code
description: 'Investigating and auditing existing work (harness variant of the built-in /code-review): review a diff or PR, audit a transcript against the evidence loop, judge a claimed-done (VERIFIED/REFUTED), scan tech debt, review a skill or plugin; discipline, performance, cross-boundary and contract lenses plus `claude-code review` detectors. Triggers "review my code", "review this PR", "review my diff", "audit this", "judge this", "is that claim actually verified", "vet this plan", "run the trap-suite eval", "check complexity", "is this slow", "N+1", "tech debt".'

metadata:
  version: 4.0.0
---

# Evaluate Code

**Investigating and auditing existing work** — code **quality** review plus the discipline lens
that catches how LLM-written code characteristically fails. Read-only: findings and a verdict, never edits.

## Pick your path

- **Review a diff/PR** → [Review a diff](#review-a-diff).
- **Audit a transcript** → [Audit a transcript](#audit-a-transcript) (grade the evidence loop the work
  claimed to follow against what actually happened).
- **Judge a claimed-done** → `references/judge.md` (is the claim true, not whether the code is good).
- **Scan tech debt** → `references/techdebt.md`.
- **Review a skill or plugin** → `references/codex-skill-plugin-review.md` (the SKILL/PLUGIN
  checklists `agents/codex.md` runs via `claude-code codex ask`).
- **A/B-eval a skill rule change** → `references/eval.md` (trap suite; `scripts/code-evaluator.js`).
- **Vet a task brief before implementation** → `references/task-review.md` (verdict `READY | NEEDS
  CLARIFICATION`; on another model family via `claude-code codex ask`).

## Audit a transcript

Grade the most recent completed work against the method its brief named (`Method:`), step by step: each
step marked **followed**, **skipped**, or **faked** (claimed without observation).

1. Run the detectors first (mechanical pass) — [Detectors](#detectors).
2. For every skip or fake, name the concrete risk it created, via `references/failure-modes.md`
   (symptom → the loop step that prevents it).
3. Score the work on the 0–2 rubric in `references/judge.md`.
4. Deliver a short table plus the single highest-value fix. Apply it only if asked.

## Lenses

Report only findings on changed code at >80% confidence; consolidate similar findings; skip stylistic preferences
unless CRITICAL.

### Discipline — the four principles

The four characteristic LLM code failures. CLAUDE.md → Behavioral guidelines defines them; what
is review-specific is how each shows up as a finding and its severity. Apply to any non-trivial
change (>20 lines), unfamiliar code, or anything a human will review; use judgment on typo and
one-line fixes. Detector per principle: the [Detectors](#detectors) table.

| Principle | As a review finding | Severity |
|---|---|---|
| **1 Think Before Coding** | silently picked one reading of an ambiguous requirement — that's a **question**, not a decision | should-fix |
| **2 Simplicity First** | *would a senior engineer call this overcomplicated?* This skill only **flags**; the fix belongs to the cleanup pass that follows review | should-fix |
| **3 Surgical Changes** | drive-by reformatting, comment churn, refactors of untouched code | nit — **should-fix** if they hide the real change |
| **4 Goal-Driven Execution** | success criteria not concrete, or not actually verified | should-fix |

Full treatment, attribution, worked examples: `references/principles.md`.

### Performance

When a change touches a query, a loop over data, or a hot path — or the review is *about*
speed/memory — apply the performance lens: **measure before you optimize**, and rank findings
by measured impact (a finding without a number is a hypothesis). The most common real target
is the **N+1 query**. Detector tables (symptom→fix, algorithmic complexity, report format) +
common mistakes: `references/performance.md`.

### Cross-boundary

When a change touches a seam (a function signature, argv/env builder, flag, file format, or an
API another package calls) — read the producer and every consumer together and exercise each
consumer path, not just the changed package; a seam is only verified on every caller.

### PR / API contract

For a PR/diff over ~200 lines, or one touching a shared module, an API serializer/schema, a DB
migration, or a table other jobs read: run **blast-radius** (who consumes the changed file sets
the bar) and **breaking-change** detection (adding is usually safe; removing/renaming/retyping/
tightening usually isn't). Commands + the safe-vs-breaking table + the migration two-phase rule:
`references/pr-and-api-review.md`.

## Detectors

`claude-code review <sub>` verbs, all support `--json`, all exit 0 (advisory signal — never a
hard gate). Run the mechanical gate first; non-zero is must-fix (`rules/standards/quality.md` →
Verification Order). Full CLI reference: `references/detectors.md`.

Verb selection: [Detector index](references/detectors.md#detector-index).

## Review a diff

This skill is what the built-in `/code-review`'s quality reviewer loads.

Determine scope:

```bash
BASE_SHA=$(git merge-base HEAD main); git diff ${BASE_SHA}...HEAD --stat
```

Mechanical gate first: `rules/standards/quality.md` → Verification Order.

1. Run the detectors on the changed files (advisory; commands in `references/detectors.md`).
2. Apply the lenses to the diff range + changed files, not the session history; verify every finding by
   reading the code.
3. Pre-approval checklist and the CRITICAL/HIGH/MEDIUM/LOW → action mapping: `rules/standards/quality.md`
   (Code Quality Checklist) · `references/completion-criteria.md` (severity) — approve when no CRITICAL/HIGH,
   block on either.
4. A change on the must-review list (`rules/standards/security.md` → Must-review) also needs a security
   pass — record that as a finding; the security pass is not this skill's.

## Verdicts

The verdict word comes first, alone, from a closed vocabulary; evidence follows. Unre-runnable claims
are UNVERIFIABLE, never assumed. Never soften a failing verdict or inflate a caveat.

This skill's own verdict, for "is the code good": **SHIP / FIX REQUIRED / BLOCK**.

| Verdict | Condition |
|---|---|
| **SHIP** | No CRITICAL or HIGH |
| **FIX REQUIRED** | HIGH present, no CRITICAL |
| **BLOCK** | Any CRITICAL |

A different question — **is the claim true** — gets a different vocabulary: `references/judge.md`'s
VERIFIED / VERIFIED WITH CAVEATS / REFUTED.

Per mode in this skill:

| Mode | Vocabulary |
|---|---|
| review (this file) | `SHIP \| FIX REQUIRED \| BLOCK` |
| judge (`references/judge.md`) | `VERIFIED \| VERIFIED WITH CAVEATS \| REFUTED` |
| task-brief / plan review (`references/task-review.md`) | `READY \| NEEDS CLARIFICATION` |
| vendor diff review (`claude-code <v> review`) | `SHIP IT \| NEEDS WORK \| BLOCK` |

## Enforcement

Four levels — passive (skill loads) → active (`/code-review`) → opt-in gate
(`claude-code review gate` via git pre-commit or PreToolUse) → CI. The gate is **opt-in by
design** — it is not wired into the generated `settings.json`. Setup for each:
`references/enforcement-patterns.md`.
