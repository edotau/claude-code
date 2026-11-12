---
paths:
  - "skills/subagent-workflows/**"
  - "agents/*.md"
  - "workflows/*.js"
---
# Master Workflow — End-to-End Development Pipeline

The pipeline for non-trivial work; Phase 3 research may run beside Phase 4 planning and joins at review.
Dispatch follows `CLAUDE.md` guideline 3; brief and relay rules: `skills/subagent-workflows/SKILL.md`.
Skip phases outside the requested scope, including commits for an edit-only task.

```text
CLASSIFY → REVIEW → CODEBASE INTEL → RESEARCH → PLAN → [REFINE: opt-in] → DISPATCH → MERGE → VERIFY → COMMIT
```

| Phase | Agent / command | Output | Gate |
|-------|----------------|--------|------|
| 0. Classify | inline | Scope tier | Picks the phases below |
| 1. Review | `Plan`, or a `codex` leaf | Refined Task Brief | READY / NEEDS CLARIFICATION |
| 2. Codebase intel | Scoped `Explore` leaves, or one `gemini` sweep | Codebase Context Brief | Structure, conventions, deps covered |
| 3. Research | `gh search`, `skills/web-explorer/SKILL.md`, or a `gemini` research leaf | Prior art + library refs | ≥1 reference, or "nothing found" stated |
| 4. Plan | plan mode or `Plan` (plan-only) | Plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` | Self-contained brief per sub-task | Cold-start test; only when the owner requests refinement |
| 6. Dispatch | `skills/subagent-workflows/SKILL.md` | Completed sub-tasks | Each passes its own check |
| 7. Merge | `claude-code worktree merge` (if a worktree was used) | Merged diff | Clean merge, no leaf still running |
| 8. Verify | `rules/standards/quality.md` → Verification Order (simplify is step 5) | Simplified, verified diff | Verification Order green |
| 9. Commit | Authorized LAST-MILE `copilot` leaf (or main-session git) | Commit pushed on branch | Clean tree, pushed |

## Phase 0: Classify

| Scope | Signal | Pipeline |
|-------|--------|----------|
| TRIVIAL | Single-line fix, typo, rename | Inline — the only inline scope; skip all phases |
| LOW | One function or file | Dispatch (not inline); phases 2, 3, 8 |
| MEDIUM | 2–5 files or needs tests, one module | Phases 1–4, 8–9; sequential unless 2+ parallel-safe tasks, then fan out via Phase 6; no worktrees |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased across sessions |

`code-engineer`'s tiers size a job it was already asked to run; TRIVIAL with no workflow request still goes inline.

## Phase 1: Adversarial review (MEDIUM+)

Vet the task request before code is written. Checklist and verdict discipline:
`skills/evaluate-code/references/task-review.md`; agent choice: `skills/subagent-workflows/references/cost-routing.md`.
The same pass on the finished diff (SHIP IT / NEEDS WORK / BLOCK) is `claude-code codex review [--adversarial]
--base <branch>`, around Phase 8.

## Phase 2: Codebase intel (all but TRIVIAL)

Three parallel `Explore` agents — Structure (layout, entry points), Conventions (naming, errors, tests),
Dependencies (deps, env vars, CLIs) — produce one brief passed to every downstream agent. **Scope the fan-out to the
directories the task touches**: a repo-wide sweep is the biggest avoidable token cost. Skip if a brief exists this
session and nothing changed. A `gemini` leaf can return a read-only multi-file sweep in one call; the manager owns any
fan-out.

## Phase 3: Research (new implementation, not pure refactor)

`gh search code|repos` → library docs and web search through `skills/web-explorer/SKILL.md` (Tavily, every agent) →
package registries (prefer battle-tested libraries) → a `gemini` leaf for cited web and deep research.
Run it beside Phase 4, not before: dispatch the research leaf (`gemini`, fallback `claude` with web-explorer) and
`Plan` in one message, both in the background; relay the findings to the running `Plan` with `SendMessage`, or
merge them into the draft at review if `Plan` finished first.

## Phase 4: Plan (MEDIUM+)

Feed both briefs to plan mode or `Plan` (rules `agents/Plan.md`, shape `docs/templates/plan.md`), then review the draft
before asking for approval: in-context for MEDIUM, a `codex` leaf for HIGH+, against
`skills/evaluate-code/references/task-review.md` → Plan review. **Gate: user approval.**

## Phase 5: Refine (owner opt-in)

For sub-tasks going to fresh agents: one brief each, with the fields in `skills/subagent-workflows/SKILL.md` →
Handoff contract. Test: a fresh agent can act from the brief alone.

## Phase 6: Dispatch (2+ parallel-safe sub-tasks)

Shape, leaf types, test agent and concurrency: `skills/subagent-workflows/SKILL.md`; pattern → shape:
`skills/subagent-workflows/references/patterns.md`.

## Phase 7: Merge

Default is no worktree (`rules/standards/worktree-git.md`); if one was used, merge per
`skills/using-git-worktrees/SKILL.md`.
After a merge, stop any leaf still running on that work; a leaf at its turn limit reports partial work without
committing.

## Phase 8: Verify

Chain: `rules/standards/quality.md` → Verification Order; loop and caps: `skills/problem-solving/references/verify.md`.
Step 5 runs once, over the **merged** diff — parallel lanes duplicate each other's helpers and only the merged diff
shows it. Same pass, before any new test: if existing tests already cover the change add nothing; delete dead code
and stale tests the change left behind; fold near-duplicate tests. Net test count may fall.
**Never edit a migration to make a gate pass.** `Workflow green-keeper-loop` only when the user asks.

## Phase 9: Commit

Only when authorized, follow `rules/standards/worktree-git.md` and `CLAUDE.md` hard rule 5.
A LAST-MILE `copilot` leaf may execute the approved commit; the main session owns release checks and push.

## Quick reference

| Scenario | Path |
|----------|------|
| Bug fix (LOW) | fix → live re-run → regression test → authorized commit |
| One-module feature (MEDIUM) | review → plan → implement → verify (simplify is its step 5) → commit |
| Cross-module feature (HIGH) | review → plan → dispatch (`code-engineer` if the user asks for a workflow) → verify → commit |
| Tech debt | `skills/evaluate-code/SKILL.md` → Tech debt |
| Review current changes | `/code-review` (security: built-in `/security-review`, `rules/standards/security.md`) |
| Second opinion | a `codex` leaf |

Saved workflows (`workflows/*.js`) run only when the user asks:
`skills/subagent-workflows/references/saved-workflows.md`.
