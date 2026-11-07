---
paths:
  - "skills/subagent-workflows/**"
  - "marketplace/plugins/code-worker/skills/code-worker/SKILL.md"
  - "agents/*.md"
  - "workflows/*.js"
---
# Master Workflow — End-to-End Development Pipeline

The pipeline for every non-trivial task; each phase gates the next.

```text
CLASSIFY → REVIEW → CODEBASE INTEL → RESEARCH → PLAN → [REFINE: opt-in] → DISPATCH → MERGE → VERIFY → COMMIT
```

| Phase | Agent / command | Output | Gate |
|-------|----------------|--------|------|
| 0. Classify | inline | Scope tier | Picks the phases below |
| 1. Review | `Plan`, or another model family via `claude-code agents ask --agent <a>` | Refined Task Brief | READY / NEEDS CLARIFICATION |
| 2. Codebase intel | 3 parallel `Explore`, or `@agent-gemini` (fans out `gemini explore` jobs) | Codebase Context Brief | Structure, conventions, deps covered |
| 3. Research | `gh search`, `skills/web-explore/SKILL.md` (Tavily), `gemini explore` | Prior art + library refs | ≥1 reference, or "nothing found" stated |
| 4. Plan | plan mode or `Plan` (plan-only) | Plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` | Self-contained brief per sub-task | Cold-start test; only with `--refine` |
| 6. Dispatch | `skills/subagent-workflows/SKILL.md` | Completed sub-tasks | Each passes its own check |
| 7. Merge | `claude-code worktree merge` (if a worktree was used) | Merged diff | Clean merge, no leaf still running |
| 8. Verify | `rules/standards/quality.md` → Verification Order (simplify is step 5) | Simplified, verified diff | Verification Order green |
| 9. Commit | `@agent-copilot commit` (or plain git) | Commit pushed on branch | Clean tree, pushed |

## Phase 0: Classify

| Scope | Signal | Pipeline |
|-------|--------|----------|
| TRIVIAL | Single-line fix, typo, rename | Inline — the only inline scope; skip all phases |
| LOW | One function or file | Dispatch (not inline); phases 2, 3, 8 |
| MEDIUM | 2–5 files or needs tests, one module | Phases 1–4, 8–9; sequential unless 2+ parallel-safe tasks, then fan out via Phase 6; no worktrees |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased across sessions |

`orchestrate`'s tiers size a job it was already asked to run; TRIVIAL with no workflow request still goes inline.

Skip the pipeline for typos, single-line changes, git operations, docs-only edits, or "just do it".

## Phase 1: Adversarial review (MEDIUM+)

Vet the task request before code is written. Checklist and verdict discipline:
`skills/evaluate-code/references/task-review.md`; agent choice: `skills/subagent-workflows/references/cost-routing.md`.
The same pass on the finished diff (SHIP IT / NEEDS WORK / BLOCK) is `claude-code codex review [--adversarial]
--base <branch>`, around Phase 8.

## Phase 2: Codebase intel (all but TRIVIAL)

Three parallel `Explore` agents — Structure (layout, entry points), Conventions (naming, errors, tests),
Dependencies (deps, env vars, CLIs) — produce one brief passed to every downstream agent. **Scope the fan-out to the
directories the task touches**: a repo-wide sweep is the biggest avoidable token cost. Skip if a brief exists this
session and nothing changed. `@agent-gemini` gives the same brief off Claude's budget: it fans out up to 12
`gemini explore` jobs (code + web) and merges them; `ask --agent gemini --tools --dirs <d>` for whole-repo context.

## Phase 3: Research (new implementation, not pure refactor)

`gh search code|repos` → library docs and web search through `skills/web-explore/SKILL.md` (Tavily, every agent) →
package registries (prefer battle-tested libraries) → `@agent-gemini` for cited web and deep research.

## Phase 4: Plan (MEDIUM+)

Feed both briefs to plan mode or `Plan` (rules `agents/Plan.md`, shape `docs/templates/plan.md`), then review the draft
before asking for approval: in-context for MEDIUM, `claude-code codex ask` (or a `coworker`) for HIGH+, against
`skills/evaluate-code/references/task-review.md` → Plan review. **Gate: user approval.**

## Phase 5: Refine (opt-in, `--refine`)

For sub-tasks going to fresh agents: one brief each, with the fields in `skills/subagent-workflows/SKILL.md` →
Handoff contract. Test: a fresh agent can act from the brief alone.

## Phase 6: Dispatch (2+ parallel-safe sub-tasks)

Shape, leaf types, test agent and concurrency: `skills/subagent-workflows/SKILL.md`; pattern → shape:
`skills/subagent-workflows/references/patterns.md`.

## Phase 7: Merge

Default is no worktree (`rules/standards/worktree-git.md`); if one was used, merge per
`skills/using-git-worktrees/SKILL.md`. Never `stash`/`checkout`/`reset` the live tree.
After a merge, stop any leaf still running on that work; a leaf at its turn limit commits or reports what's uncommitted.

## Phase 8: Verify

Chain: `rules/standards/quality.md` → Verification Order; loop and caps: `skills/problem-solving/references/verify.md`.
Step 5 runs once, over the **merged** diff — parallel lanes duplicate each other's helpers and only the merged diff
shows it. Same pass, before any new test: if existing tests already cover the change add nothing; delete dead code
and stale tests the change left behind; fold near-duplicate tests. Net test count may fall.
**Never edit a migration to make a gate pass.** `Workflow green-keeper-loop` only when the user asks.

## Phase 9: Commit

Per `rules/standards/worktree-git.md`; `@agent-copilot commit` executes.
Then push the current branch (CLAUDE.md hard rule 5) — `commit` does not push.

## Quick reference

| Scenario | Path |
|----------|------|
| Bug fix (LOW) | fix → live re-run → regression test → `@agent-copilot commit` |
| One-module feature (MEDIUM) | review → plan → implement → verify (simplify is its step 5) → commit |
| Cross-module feature (HIGH) | review → plan → dispatch (`orchestrate` if the user asks for a workflow) → verify → commit |
| Tech debt | `skills/evaluate-code/SKILL.md` → Tech debt |
| Review current changes | `/code-review` (security: built-in `/security-review`, `rules/standards/security.md`) |
| Second opinion | `@agent-codex` |

Saved workflows (`workflows/*.js`) run only when the user asks:
`skills/subagent-workflows/references/saved-workflows.md`.
