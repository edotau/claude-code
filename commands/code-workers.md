---
description: "Fan out code-worker subagents to finish a series of tasks — parallel (independent), sequential (plan with review gate per task), or auto. The multi-leaf counterpart to a single Agent dispatch."
argument-hint: "[parallel | sequential | auto | team] <task1 | task2 | ...>  (tasks '|'-separated; blank reads the approved plan)"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

Launcher only. The dispatch shape, gates, and report format live in
`skills/subagent-workflows/SKILL.md` (`references/dispatching-parallel-agents.md` for parallel,
`SKILL.md` → Sequential plan for sequential).

Strip an optional leading mode word (`parallel`/`sequential`/`auto`/`team`) from `$ARGUMENTS`; split the
rest on `|` into the task list. Blank task list: read the most recent
`~/.claude/plans/<repo>/*.md` (or `~/.claude/plans/*.md` for this repo) and use its task
breakdown. `auto` or no mode: infer parallel for independent tasks, sequential for
coupled/ordered/plan-derived ones, per the skill's work-shape table — state the choice and why.

Dispatch each task as a `code-worker` leaf per the skill's process, each with a `copilot` test agent
(`rules/standards/quality.md`). Run the spec → simplify → quality
gates from the skill before moving to the next task (sequential) or over the integrated diff
(parallel). Relay one consolidated report: per-task status, files, verification, overall verdict.

Default agents: when the plan or task names no agent, set each leaf's `Agent:` from the "Default agent per
task class" table in `skills/subagent-workflows/references/cost-routing.md`,
or in-context when cost-routing says so.

`team` mode (`/code-workers team <goal>`): `code-worker` leaves with `Agent: gemini` scan (read-only, cites
file:line) → `Agent: codex` reviews the list (KEEP/CHANGE/DROP) → STOP and show the reviewed list for the user's
approval → approved items go to `code-worker` leaves with `Agent: opencode` (worktree) ‖ a `copilot` test agent
when code changes. Watch each dispatch with `claude-code <agent> tail -f`.
