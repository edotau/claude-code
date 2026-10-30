---
paths:
  - "skills/subagent-workflows/**"
  - "commands/copilot.md"
  - "commands/code-workers.md"
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
| 1. Review | `Plan`, or another model family via `claude-code ask --agent <a>` | Refined Task Brief | READY / NEEDS CLARIFICATION |
| 2. Codebase intel | 3 parallel `Explore`, or one `claude-code gemini explore` | Codebase Context Brief | Structure, conventions, deps covered |
| 3. Research | `gh search`, WebFetch, WebSearch, `gemini explore` | Prior art + library refs | ≥1 reference, or "nothing found" stated |
| 4. Plan | plan mode or `Plan` (plan-only) | Plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` | Self-contained brief per sub-task | Cold-start test; only with `--refine` |
| 6. Dispatch | orchestrate / `/code-workers`, `code-worker` per disjoint file set | Completed sub-tasks | Each passes its own check |
| 7. Merge | `claude-code worktree merge` (if a worktree was used) | Merged diff | Clean merge, no leaf still running |
| 8. Verify | `rules/standards/quality.md` → Verification Order (simplify is step 5) | Simplified, verified diff | Verification Order green |
| 9. Commit | `/copilot commit` (or plain git) | Commit on branch | Clean tree |

## Phase 0: Classify

| Scope | Signal | Pipeline |
|-------|--------|----------|
| TRIVIAL | Single-line fix, typo, rename | Inline — the only inline scope; skip all phases |
| LOW | One function or file | Dispatch (not inline); phases 2, 3, 8 |
| MEDIUM | 2–5 files or needs tests, one module | Phases 1–4, 8–9; sequential, no worktrees |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased across sessions |

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
session and nothing changed. `gemini explore` (or `bridge` for whole-repo context) gives the same brief off Claude's
budget.

## Phase 3: Research (new implementation, not pure refactor)

`gh search code|repos` → library docs via WebFetch → WebSearch only if those fall short → package registries
(prefer battle-tested libraries) → `gemini explore` for cited web research.

## Phase 4: Plan (MEDIUM+)

Feed both briefs to plan mode or `Plan`; the template is `agents/Plan.md`. Break down tasks, mark dependencies and
`parallel-safe: true/false`, flag migrations, breaking changes and security surface. **Gate: user approval.**

## Phase 5: Refine (opt-in, `--refine`)

For sub-tasks going to fresh agents. Each brief: Objective, Context (paths + lines), Constraints, Steps, Acceptance
Criteria, Do NOTs, Verification Command. Test: a fresh agent can act from the brief alone
(`skills/subagent-workflows/SKILL.md`).

## Phase 6: Dispatch (2+ parallel-safe sub-tasks)

Each sub-task is a `code-worker` owning disjoint (ideally new) files in the shared tree, with `copilot` in the SAME
message when a test agent is needed (`standards/testing-guidelines.md`). Another model family for one lane:
`Agent: <x>` in the brief ([Leaf types](../../skills/subagent-workflows/SKILL.md#leaf-types)).
Cap: `agents.md` → Parallel dispatch.

## Phase 7: Merge

Default is no worktree (`rules/standards/worktree-git.md`); if one was used, merge per
`skills/using-git-worktrees/references/merge-and-clean.md`. Never `stash`/`checkout`/`reset` the live tree.
After a merge, stop any leaf still running on that work; a leaf at its turn limit commits or reports what's uncommitted.

## Phase 8: Verify

Chain: `rules/standards/quality.md` → Verification Order; loop and caps: `skills/problem-solving/references/verify.md`.
Step 5 runs once, over the **merged** diff — parallel lanes duplicate each other's helpers and only the merged diff
shows it. Same pass, before any new test: if existing tests already cover the change add nothing; delete dead code
and stale tests the change left behind; fold near-duplicate tests. Net test count may fall.
**Never edit a migration to make a gate pass.** `Workflow green-keeper-loop` only when the user asks.

## Phase 9: Commit

Per `rules/standards/worktree-git.md`; `/copilot commit` executes, `/copilot pr` when a PR is requested.

## Quick reference

| Scenario | Path |
|----------|------|
| Bug fix (LOW) | fix → live re-run → regression test → `/copilot commit` |
| One-module feature (MEDIUM) | review → plan → implement → verify (simplify is its step 5) → commit |
| Cross-module feature (HIGH) | review → orchestrate `[--refine]` → verify → commit |
| Tech debt | `skills/evaluate-code/references/techdebt.md` |
| Review current changes | `/code-review` (security is an `evaluate-code` lens, not an agent) |
| Second opinion | `/codex` |

Saved workflows (`workflows/*.js`) run only when the user asks:
`skills/subagent-workflows/references/saved-workflows.md`.
