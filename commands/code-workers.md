---
description: "Fan out code-worker subagents to finish a series of tasks — parallel (independent), sequential (plan with review gate per task), or auto. The multi-leaf counterpart to a single Agent dispatch."
argument-hint: "[parallel | sequential | auto] <task1 | task2 | ...>  (tasks '|'-separated; blank reads the approved plan)"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

Launcher only. The dispatch shape, gates, and report format live in
`skills/subagent-workflows/SKILL.md` (`references/dispatching-parallel-agents.md` for parallel,
`references/subagent-driven-development.md` for sequential).

Strip an optional leading mode word (`parallel`/`sequential`/`auto`) from `$ARGUMENTS`; split the
rest on `|` into the task list. Blank task list: read the most recent
`~/.claude/plans/<repo>/*.md` (or `~/.claude/plans/*.md` for this repo) and use its task
breakdown. `auto` or no mode: infer parallel for independent tasks, sequential for
coupled/ordered/plan-derived ones, per the skill's work-shape table — state the choice and why.

Dispatch each task as a `code-worker` leaf per the skill's process, with a `copilot` test agent in
the SAME message every time (`rules/standards/quality.md`). Run the spec → simplify → quality
gates from the skill before moving to the next task (sequential) or over the integrated diff
(parallel). Relay one consolidated report: per-task status, files, verification, overall verdict.
