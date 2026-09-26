---
name: planning
description: Implementation planning, task breakdown, multi-session blueprints. Triggers on "write an implementation plan", "break down this task", "blueprint", multi-step feature work.

metadata:
  version: 1.0.0
---

# Planning

Three modes: writing implementation plans, generating construction blueprints, and executing plans.

Invoke this skill (`/planning`) for Mode 1, add `--blueprint <task>` for Mode 2, `--execute <path>` for Mode 3. Plan mode or a `Plan` subagent is the default executor for Mode 1 and follows the rules below.

## Mode 1: Write Implementation Plans

### Task Breakdown Rules

Every task in a plan must be:
- **Bite-sized**: 2-5 minutes of work per step
- **Verifiable**: Has a RED (failing test) and GREEN (passing) checkpoint
- **Self-contained**: Can be understood without reading other tasks
- **No placeholders**: Every step specifies exactly what to do — no "implement as needed"

### Plan Structure

```markdown
# Plan: <Feature Name>

## Context
<Why this exists, what problem it solves>

## Tasks

### Task 1: <Verb> <Object>
**Files**: `path/to/file.py` (lines X-Y)
**RED**: Write test that <expected behavior>. Run: `.venv/bin/pytest tests/test_foo.py::test_bar -x -v -n0`. Expect FAIL.
**GREEN**: Implement <specific change>. Run same test. Expect PASS.
**Verify**: `make lint && make test-unit`

### Task 2: <Verb> <Object>
...
```

Plans are written to `.claude/docs/plans/<kebab-slug>.md` by default — never a repo root. Use the existing `.claude/docs/plans/stages/` directory when the plan is part of an `/workflow:orchestrate` pipeline — the stage handoff file already lives there.

### Self-Review Checklist

Before finalizing a plan:
- [ ] Every task has a concrete verb (Add, Create, Modify, Extract, Remove)
- [ ] Every task lists specific files and line ranges
- [ ] No task takes more than 5 minutes
- [ ] No placeholders ("implement as needed", "add appropriate logic")
- [ ] Dependencies between tasks are explicit
- [ ] Parallel-safe tasks are marked
- [ ] Verification commands use `.venv/bin/` prefix — never bare `python`/`pytest`

**Detector pass (optional but recommended).** Pipe the drafted plan text through the two
plan-text detectors (stdin; they analyze PLAN TEXT, never code files — full CLI:
`skills/evaluate-code/references/detectors.md`):

```bash
cat .claude/docs/plans/<slug>.md | claude-code review assumptions - --json   # #1 hidden assumptions
cat .claude/docs/plans/<slug>.md | claude-code review goals - --json         # #4 verification quality
```

A `CLARIFY` (assumptions) or `MISSING` (verification) verdict means strengthen the plan
before dispatching it — surface the assumption or add a concrete check to the weak step.

For adversarial review of a finished plan, dispatch the reviewer template at the end of this skill.

## Mode 2: Generate Blueprints

For multi-session projects that need a construction plan.

### Blueprint Pipeline

```
RESEARCH (Explore agents) → DESIGN (interfaces, data flow, parallel/deps) →
DRAFT (task-breakdown rules above, cold-start executable) →
REVIEW (adversarial: edge cases, dep order, assumed context, over-large steps) →
REGISTER (save to .claude/docs/plans/YYYY-MM-DD[-<repo>]-<slug>.md)
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
- Plan mode / the `Plan` subagent — Mode 1 executor
- `references/writing-plans-detailed.md` — detailed plan-header/checkbox templates
- `/workflow:orchestrate` — pipeline that consumes plans at the `plan` stage
- `rules/workflow/master-workflow.md` Phase 1 — adversarial pre-review; run it on MEDIUM+ tasks before planning
