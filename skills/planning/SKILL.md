---
name: planning
description: Implementation planning, task breakdown, multi-session blueprints. Triggers on "write an implementation plan", "break down this task", "blueprint", multi-step feature work.

metadata:
  version: 1.0.0
---

# Planning

Three modes: writing implementation plans, generating construction blueprints, and executing plans.

Invoke this skill (`/planning`) for Mode 1, add `--blueprint <task>` for Mode 2, `--execute <path>` for Mode 3. Plan mode or a `Plan` subagent (`agents/Plan.md`) is the executor for Mode 1.

## Mode 1: Write Implementation Plans

**Canonical source: [`agents/Plan.md`](../../agents/Plan.md)** — the `Plan` subagent (it overrides Claude Code's
built-in Plan agent, so plan mode uses it too). It holds the plan template (Context → Phases → Risks → `## Tasks`),
the phase rules, the self-review checklist, the detector pass, and where plans are filed
(`~/.claude/plans/[<repo-slug>/]YYYY-MM-DD-<slug>.md`, via the `save-plan` hook). Edit Mode 1 there, not here.
`/workflow:orchestrate` stage handoffs sit beside plans as `~/.claude/plans/stages/YYYY-MM-DD-<task-id>.md`.
The `plan-template` gate and `plan-mirror` hook keep plan mode's native undated draft mirrored to that dated
name from its first write, so it is never the file of record even mid-draft.
Every `## Tasks` line is leaf-sized — one package (or ≤3 tightly coupled files), ≤10 findings/edits, named
`runner:` and `owns:` — so more agents can fan out and none dies mid-task.

## Mode 2: Generate Blueprints

For multi-session projects that need a construction plan.

### Blueprint Pipeline

```
RESEARCH (Explore agents) → DESIGN (interfaces, data flow, parallel/deps) →
DRAFT (phase rules in agents/Plan.md, cold-start executable) →
REVIEW (adversarial: edge cases, dep order, assumed context, over-large steps) →
REGISTER (save to ~/.claude/plans/[<repo-slug>/]YYYY-MM-DD-<slug>.md)
```

### Cold-Start Execution

Every step in a blueprint must be executable by a fresh agent with zero prior context. Include:
- File paths and line numbers
- Exact commands to run (`.venv/bin/pytest …`, `make …`)
- Expected output
- What "done" looks like

### Plan Mutation Protocol

Plans evolve during execution. When changing a plan:
1. Document what changed and why
2. Re-check dependency graph
3. Update parallel-safe markers
4. Notify if scope expanded

## Mode 3: Execute Plans

Load the plan, re-check its steps against current codebase state, execute in dependency order
(parallel-safe tasks together), run each task's verification command before moving on, and track
progress with TaskCreate/TaskUpdate.

STOP and ask the user when a step's assumptions are wrong (missing file, changed API), scope is
larger than expected, a dependency is blocked, or a decision falls outside the plan.

### Handoff Patterns

**Subagent-driven**: Each plan task dispatched to a fresh subagent with its refined prompt. Use for parallel-safe tasks. See the `subagent-workflows` skill (`references/dispatching-parallel-agents.md`).

**Inline execution**: Execute tasks sequentially in the current session. Use for dependent tasks or small plans.

**Worktree dispatch**: Each task gets its own git worktree. Use for large cross-module changes. See `/workflow:worktree` and `/workflow:orchestrate`.

## Plan Document Reviewer

When dispatching a subagent to review a written plan (completeness, spec alignment, task
decomposition, buildability), use the ready-to-paste dispatch prompt in
[references/reviewer-prompt-template.md](references/reviewer-prompt-template.md). Reviewer
returns Status (Approved | Issues Found) + Issues + advisory Recommendations.

## Related

- `/planning` — this skill; entry point for all three modes
- `agents/Plan.md` — Mode 1 (plan template, phase rules, self-review); plan mode and the `Plan` subagent
- `/workflow:orchestrate` — pipeline that consumes plans at the `plan` stage
- `rules/workflow/master-workflow.md` Phase 1 — adversarial pre-review; run it on MEDIUM+ tasks before planning
