---
description: "Workhorse fan-out: claude and copilot code-worker leaves finish a series of tasks — parallel, sequential (review gate per task) or auto. Other agents and model discovery: /coworkers."
argument-hint: "[agent=claude|copilot] [profile=p] [parallel|sequential|auto] task|…"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

Launcher only. The dispatch shape, gates, and report format live in
`skills/subagent-workflows/SKILL.md` (Ownership for parallel, Sequential plan for sequential).

`$ARGUMENTS` = `help`: print the Usage block below verbatim and stop.

Strip an optional leading mode word (`parallel`/`sequential`/`auto`) from `$ARGUMENTS`; split the
rest on `|` into the task list. Blank task list: read the most recent
`~/.claude/plans/<repo>/*.md` (or `~/.claude/plans/*.md` for this repo) and use its task
breakdown. `auto` or no mode: infer parallel for independent tasks, sequential for
coupled/ordered/plan-derived ones, per the skill's work-shape table — state the choice and why.

Dispatch each task as a `code-worker` leaf per the skill's process, feature/bug-fix tasks add a `copilot` test agent
(`rules/standards/testing-guidelines.md`). Run the spec → simplify → quality
gates from the skill before moving to the next task (sequential) or over the integrated diff
(parallel); parallel leaves follow SKILL.md → Concurrency (≤2 while another session shares the checkout).
Relay one consolidated report: per-task status, files, verification, overall verdict.

Default agents: the pool is claude (in-context) and copilot only. An `agent=` token (stripped first) sets every
leaf's `Agent:`; without it, copilot takes mechanical edits and test repair and claude the rest. Tasks for codex,
gemini or opencode, or a discovered model, go through `/coworkers`.

A `profile=<p>` token (stripped first) adds `Profile: <p>` to every leaf brief, as in `/coworkers`.

The scan → review → edit pipeline across gemini, codex and opencode is `/coworkers team`.

## Usage

```text
/code-workers [agent=claude|copilot] [profile=<p>] [parallel|sequential|auto] <task> [| <task> …]

  agent=    claude (in-context; reasoning, integration) · copilot (edits, mechanical work, tests)
            omitted = per task: copilot for mechanical edits and test repair, claude for the rest
  profile=  vault profile that bills the copilot leaves (dev, prod, …); omitted = the session profile
  mode      auto (default: parallel if independent, sequential if coupled or from a plan) · parallel ·
            sequential (spec → simplify → quality gate per task); team mode moved to /coworkers team
  no task   runs the newest plan's task list (~/.claude/plans/…)

Feature and bug-fix tasks also get a copilot test agent in the same message.

Examples:
  /code-workers fix the watchdog flake | rename ResolveEnv to CanonicalizeEnv   auto, two leaves
  /code-workers sequential                                                     newest plan, gated per task
  /code-workers agent=copilot parallel add ctx timeouts to internal/usage | …   copilot leaves
  /code-workers profile=prod agent=copilot regenerate the settings golden       copilot billed to prod
```
