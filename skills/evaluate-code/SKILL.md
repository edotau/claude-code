---
name: evaluate-code
description: 'Use when reviewing code, a PR, or a diff for quality (harness variant of the built-in /code-review) — or receiving review feedback. Covers the four-principle discipline lens (think-before-coding, simplicity, surgical changes, goal-driven), a performance lens (N+1, hot paths, memory), and stdlib detectors. Triggers "review my code", "review this PR", "review my diff", "check complexity", "am I overcomplicating this", "karpathy check", "review feedback", "is this slow", "check performance", "N+1". For the security pass, see the security-reviewer skill.'

metadata:
  version: 3.2.0
---

# Code Review

Code **quality** review plus the discipline lens that catches how LLM-written code
characteristically fails. **Security is a separate, parallel pass** — the `security-reviewer`
skill owns it. The built-in `/code-review` fans out **both** (quality + security) and
synthesizes one verdict.

Pick your path:
- **Reviewing a diff/PR** → [Dispatch a review](#dispatch-a-review) (fan out both reviewers).
- **Reviewing by hand** → [Conduct a review](#conduct-a-review) (checklist + severity).
- **Getting review feedback** → [Receive feedback](#receive-feedback) (verify-first, no gratitude).

## The four principles (discipline lens)

The four characteristic LLM code failures. CLAUDE.md → Behavioral guidelines defines them; what
is review-specific is how each shows up as a finding and its severity. Apply to any non-trivial
change (>20 lines), unfamiliar code, or anything a human will review; use judgment on typo and
one-line fixes. Detector per principle: the [Detectors](#detectors) table.

| Principle | As a review finding | Severity |
|---|---|---|
| **1 Think Before Coding** | silently picked one reading of an ambiguous requirement — that's a **question**, not a decision | should-fix |
| **2 Simplicity First** | *would a senior engineer call this overcomplicated?* This skill only **flags**; `simplify` owns the fix | should-fix |
| **3 Surgical Changes** | drive-by reformatting, comment churn, refactors of untouched code | nit — **should-fix** if they hide the real change |
| **4 Goal-Driven Execution** | success criteria not concrete, or not actually verified | should-fix |

Full treatment + attribution: `references/karpathy-principles.md`. Before/after examples per
detector: `references/anti-patterns.md`.

## Detectors

`claude-code review <sub>` verbs, all support `--json`, all exit 0 (advisory signal — never a
hard gate). **Run the mechanical gate first** (`make fmt && make lint`) — that one *can* fail, and a
non-zero result is a must-fix before review proceeds. Full CLI reference: `references/detectors.md`.

Verb selection: [Detector index](references/detectors.md#detector-index).

## Performance lens

When a change touches a query, a loop over data, or a hot path — or the review is *about*
speed/memory — apply the performance lens: **measure before you optimize**, and rank findings
by measured impact (a finding without a number is a hypothesis). The most common real target
is the **N+1 query**. Detector tables (symptom→fix,
algorithmic complexity, report format) + common mistakes: `references/performance.md`.

## Dispatch a review

Determine scope, then fan out **both** reviewers in parallel.

Scope and mechanical-gate commands: [Dispatch setup](references/tasks.md#dispatch-setup).

1. Run the detectors on the changed files (advisory; commands in `references/detectors.md`).
2. In a **single message**, launch two subagents concurrently:
   - a **general-purpose** subagent that loads this skill — quality, correctness, the discipline lens.
   - a **general-purpose** subagent that loads the **`security-reviewer` skill** (or run
     `/security`) — OWASP Top 10, secrets, injection, auth. Read-only (findings only).
3. Scope each agent to the diff range + changed files, NOT your session history — keeps them on
   the work product and preserves your context. A ready-to-paste dispatch prompt: `references/tasks.md`.

For a PR/diff over ~200 lines, or one touching a shared module, an API serializer/schema, a DB
migration, or a table other jobs read, also run **blast-radius** (who consumes the changed file
sets the bar) and **breaking-change** detection (adding is usually safe; removing/renaming/
retyping/tightening usually isn't). Commands + the safe-vs-breaking table + the migration
two-phase rule: `references/pr-and-api-review.md`.

### Synthesize the verdict

| Verdict | Condition |
|---|---|
| **SHIP** | No CRITICAL or HIGH from either pass |
| **FIX REQUIRED** | HIGH present, no CRITICAL |
| **BLOCK** | Any CRITICAL |

## Conduct a review

Pre-approval checklist and the CRITICAL/HIGH/MEDIUM/LOW → action mapping are canonical in
`rules/standards/quality.md` (Code Quality Checklist · Review Severity) — approve when no
CRITICAL/HIGH, block on either.

**Escalate to the security pass** whenever a change touches auth, user input, DB queries,
file-system ops, external APIs, crypto, or payments — `security-reviewer` territory, not a
quality nit.

## Receive feedback

Iron rules:

1. **Verify before implementing** — reproduce the issue before changing code.
2. **YAGNI** — does the suggestion solve a real problem or a hypothetical one?
3. **Push back when justified** — if a change makes the code worse, say so with evidence
   (file, line, pattern) and propose an alternative.
4. **Order**: blocking issues (CRITICAL/HIGH) → simple fixes (typos, naming) → complex changes last.

### Replying

Reply wording and GitHub thread endpoint: [Replying](references/feedback.md#replying).

## Enforcement

Four levels — passive (skill loads) → active (`/code-review`) → opt-in gate
(`claude-code review gate` via git pre-commit or PreToolUse) → CI. The gate is **opt-in by
design** — it is not wired into the generated `settings.json`. Setup for each:
`references/enforcement-patterns.md`.
