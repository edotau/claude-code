---
name: evaluate-code
description: 'Use when reviewing code, a PR, or a diff for quality (harness variant of the built-in /code-review) — or receiving review feedback. Covers the four-principle discipline lens (think-before-coding, simplicity, surgical changes, goal-driven), a performance lens (N+1, hot paths, memory), and stdlib detectors. Triggers "review my code", "review this PR", "review my diff", "check complexity", "am I overcomplicating this", "review feedback", "is this slow", "check performance", "N+1". For the security pass, see the security-reviewer skill.'

version: 3.1.0
---

# Code Review

Code **quality** review and the discipline lens that catches how LLM-written code
characteristically fails. Security is a separate, parallel pass — the `security-reviewer`
skill owns it. `/code-review` fans out **both** (quality + security) and synthesizes one
verdict.

Three modes: dispatch a review, conduct a review, receive review feedback.

## Quick start

```bash
/code-review                              # staged/branch diff → fan out both reviewers
make fmt && make lint                     # mechanical gate FIRST (format + lint; can fail)
# Resolve skill roots: config dir → project .claude → git toplevel (harness checkout) → global.
CR=""
for base in "${CLAUDE_CONFIG_DIR:-}" "${CLAUDE_PROJECT_DIR:+$CLAUDE_PROJECT_DIR/.claude}" "$(git rev-parse --show-toplevel 2>/dev/null)" "$HOME/.claude"; do
  [ -n "$base" ] && [ -d "$base/skills/evaluate-code/scripts" ] && CR="$base/skills/evaluate-code/scripts" && break
done
SIMP=""
for base in "${CLAUDE_CONFIG_DIR:-}" "${CLAUDE_PROJECT_DIR:+$CLAUDE_PROJECT_DIR/.claude}" "$(git rev-parse --show-toplevel 2>/dev/null)" "$HOME/.claude"; do
  [ -n "$base" ] && [ -d "$base/skills/simplicity/scripts" ] && SIMP="$base/skills/simplicity/scripts" && break
done
python3 "$SIMP/complexity_checker.py" src/ --threshold medium   # #2 Simplicity (owned by simplicity)
python3 "$CR/diff_surgeon.py" --diff HEAD~1..HEAD               # #3 Surgical
echo "I'll just export all user data" | python3 "$CR/assumption_linter.py" -  # #1 Think
python3 "$CR/goal_verifier.py" plan.md                          # #4 Goal-driven
```

## The Four Principles (discipline lens)

LLM-written code fails in four characteristic ways. Apply this lens to any non-trivial
change (>20 lines), unfamiliar code, or anything humans will review. For typo/one-line
fixes, use judgment.

1. **Think Before Coding** — assumptions surfaced, not silent. A change that picked one
   interpretation of an ambiguous requirement without saying so is a **question**.
2. **Simplicity First** — minimum code that solves the problem. No single-use
   abstractions, speculative flexibility, or error handling for impossible states.
   *Would a senior engineer call this overcomplicated?* → **should-fix**. This is a
   flag-it lens here; the **apply-the-fix** treatment (patterns, extract-method playbook,
   and the `complexity_checker.py` detector) lives in the `simplicity` skill, its
   canonical owner.
3. **Surgical Changes** — every changed line traces to the task. Drive-by reformatting,
   comment churn, refactors of untouched code → **nit** (or **should-fix** if they hide
   the real change).
4. **Goal-Driven Execution** — success criteria are concrete and verified, not "looks
   right". Missing verification → **should-fix**.

Full treatment + attribution: `references/karpathy-principles.md`. Worked before/after
examples mapped to each detector: `references/anti-patterns.md`.

## What's in the box

| Piece | Where | Detail |
|-------|-------|--------|
| Lint & format gate | `make fmt` / `make lint` | mechanical format + lint, run first — **can fail** (must-fix) |
| 3 discipline detectors | `scripts/` | diff / assumption / goal — stdlib, advisory (#2 `complexity_checker.py` lives in `simplicity`) |
| Quality analyzer | `scripts/code_quality_checker.py` | smells + SOLID + score; shares `quality_core.py` |
| PR analyzer + report | `scripts/pr_analyzer.py`, `review_report_generator.py` | risk/categorization + multi-format report |
| Opt-in gate | `hooks/code-review-gate.sh` | non-blocking pre-commit awareness (exit 0) |
| Reference docs | `references/` | detectors · principles · anti-patterns · enforcement · pr-and-api-review |

## Automated Detectors

Stdlib-only Python in `.claude/skills/evaluate-code/scripts/` — except `complexity_checker.py`
(#2), which is canonical in `../simplicity/scripts/`. Run the **lint & format gate first**
(it can fail — treat non-zero as must-fix), then these detectors as advisory signal. All
support `--json`; all exit 0. Full CLI reference + the lint-gate commands:
`references/detectors.md`.

| Tool | Principle | Flags |
|------|-----------|-------|
| `complexity_checker.py` *(in `simplicity`)* | #2 Simplicity | per-function complexity/length/nesting; class density; premature ABC/Protocol (AST for Python) |
| `diff_surgeon.py` | #3 Surgical | whitespace/comment churn, docstring adds, quote-style swaps |
| `assumption_linter.py` | #1 Think | minimizing language, hopeful phrasing, absolute scope, missing verification |
| `goal_verifier.py` | #4 Goal | plan steps scored 0–3 on verification quality; missing final check |
| `code_quality_checker.py` | quality | smells, SOLID violations, 0–100 score (6 languages) |
| `pr_analyzer.py` | triage | file-risk categorization, commit-message lint, complexity score |
| `review_report_generator.py` | orchestration | combines pr + quality into one report (text/md/json) |

## Performance lens

When a change touches a query, a loop over data, or a hot path — or the review is *about*
speed/memory — apply the performance lens: **measure before you optimize**, and the top
target on this stack is the **N+1 query** (`select_related`/`prefetch_related`). Rank findings
by measured impact (a finding without a number is a hypothesis). Full detector tables
(symptom→fix, algorithmic complexity, report format) + common mistakes:
`references/performance.md` (the **performance-optimizer reference**, absorbing the retired agent).
The same procedure also serves a proactive pass (slow stage / SLA breach); this lens is the inline
review pass.

## Mode 1: Dispatch a Review

Determine scope, then fan out **both** reviewers in parallel.

```bash
BASE_SHA=$(git merge-base HEAD main)   # or: git diff --cached for staged only
git diff ${BASE_SHA}...HEAD --stat
```

1. Run the lint & format gate (`make fmt && make lint`), then the detectors on the changed
   files (advisory). A lint/format failure is a must-fix before review proceeds.
2. In a **single message**, launch two subagents concurrently:
   - **code-engineer** agent (review phase) — quality, correctness, the discipline lens.
   - a **general-purpose** subagent that loads the **`security-reviewer` skill** (or run
     `/security`) — OWASP Top 10, secrets, injection, auth. Keep it read-only (findings only).
3. Give each agent precisely scoped context (the diff range + changed files, NOT your
   session history). This keeps them on the work product and preserves your context.

### Synthesize verdict

| Verdict | Condition |
|---------|-----------|
| **SHIP** | No CRITICAL or HIGH from either pass |
| **FIX REQUIRED** | HIGH present, no CRITICAL |
| **BLOCK** | Any CRITICAL |

## PR & contract review (blast radius + breaking changes)

For a PR/diff over ~200 lines, or any change touching a shared module, DRF serializer,
Django migration, or Unity Catalog table, run two passes the detectors can't make for you:
**blast radius** (who imports/consumes the changed file — the most-exposed consumer sets
the review bar) and **breaking-change detection** (adding is usually safe; removing,
renaming, retyping, or tightening a contract usually isn't). Full commands, the
safe-vs-breaking table, and the migration two-phase rule: `references/pr-and-api-review.md`.

## Mode 2: Conduct a Review

### Checklist (before approving)

- [ ] Readable and well-named
- [ ] Functions focused (<50 lines), files cohesive (<800 lines)
- [ ] No deep nesting (>4 levels) — early returns
- [ ] Errors handled explicitly; no swallowed exceptions
- [ ] No debug statements left in
- [ ] Tests exist for new behavior; coverage ≥80%

### Severity Levels

| Level | Meaning | Action |
|-------|---------|--------|
| CRITICAL | Security vuln or data-loss risk | **BLOCK** |
| HIGH | Bug or significant quality issue | **WARN** — should fix |
| MEDIUM | Maintainability concern | **INFO** |
| LOW | Style / minor | **NOTE** |

**Escalate to the security pass** whenever a change touches auth, user input, DB queries,
file-system ops, external APIs, crypto, or payments — those are `security-reviewer`
territory, not a quality nit.

## Mode 3: Receive Review Feedback

### Iron rules

1. **Verify before implementing** — reproduce the issue before changing code.
2. **No performative agreement** — never "You're absolutely right!" / "Great catch!".
3. **YAGNI** — does the suggestion solve a real problem or a hypothetical one?
4. **Push back when justified** — if a change makes the code worse, say so with evidence
   (file, line, pattern) and propose an alternative.

### No Performative Agreement (hard rule)

When feedback is correct, fix it and state what changed — never perform gratitude:

```
✅ "Fixed. Extracted PROGRESS_INTERVAL constant."
✅ "Good catch — off-by-one in the loop bound. Fixed in foo.py:42."
❌ "You're absolutely right!"  ❌ "Great point!"  ❌ "Thanks for catching that!"  ❌ ANY thanks
```

If you catch yourself about to write "Thanks" — delete it and state the fix. The code shows
you heard it. If you pushed back and were wrong: `"You were right — I checked X and it does
Y. Fixing now."` — factual, no long apology.

### Implementation order

Blocking issues (CRITICAL/HIGH) first → simple fixes (typos, naming) → complex changes
(refactor, architecture) last.

### GitHub thread replies

Reply in the comment thread (`gh api repos/{owner}/{repo}/pulls/{pr}/comments/{id}/replies`),
not as a top-level PR comment.

## Enforcement

Four levels — passive (skill loads) → active (`/code-review`) → opt-in gate
(`hooks/code-review-gate.sh` via git pre-commit or PreToolUse) → CI. Setup for each:
`references/enforcement-patterns.md`. The gate is **opt-in by design** — it is not wired
into the generated `settings.json`.
