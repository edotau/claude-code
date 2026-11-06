---
description: "Workhorse fan-out: claude, codex and gemini code-worker leaves finish a series of tasks — parallel, sequential (review gate per task) or auto. Copilot, opencode, Databricks and model discovery: /coworker."
argument-hint: "[agent=claude|codex|gemini] [profile=p] [parallel|sequential|auto] task|…"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
disable-model-invocation: true
---

Launcher only. The dispatch shape, gates, and report format live in
`~/.claude/skills/subagent-workflows/SKILL.md` (Ownership for parallel, Sequential plan for sequential).

`$ARGUMENTS` = `help`: print the Usage block below verbatim and stop.

Strip an optional leading mode word (`parallel`/`sequential`/`auto`) from `$ARGUMENTS`; split the
rest on `|` into the task list. Blank task list: read the most recent
`~/.claude/plans/<repo>/*.md` (or `~/.claude/plans/*.md` for this repo) and use its task
breakdown. `auto` or no mode: infer parallel for independent tasks, sequential for
coupled/ordered/plan-derived ones, per the skill's work-shape table — state the choice and why.

Dispatch each task as a `code-worker:<agent>` leaf (`subagent_type: "code-worker:claude|codex|gemini"`) per the
skill's process; feature/bug-fix tasks add a `copilot` test agent (`rules/standards/testing-guidelines.md`).
Run the spec → simplify → quality gates from the skill before moving to the next task (sequential) or over the
integrated diff (parallel); parallel leaves only on disjoint files (SKILL.md → Concurrency).
Relay one consolidated report: per-task status, files, verification, overall verdict.

Default agents: the pool is claude (in-context), codex and gemini. An `agent=` token (stripped first) sets every
leaf's type; without it, per task: codex for review or a second opinion, gemini for read-heavy sweeps and research,
claude for the rest. Tasks for copilot, opencode, Databricks or a discovered model go through `/coworker`.

A `profile=<p>` token (stripped first) adds `Profile: <p>` to every leaf brief, as in `/coworker`.

The scan → review → edit pipeline across gemini, codex and opencode is `/coworker team`.

# Usage

```text
/code-worker [agent=claude|codex|gemini] [profile=<p>] [parallel|sequential|auto] <task> [| <task> …]

  agent=    claude (in-context; reasoning, integration) · codex (review, second opinion) ·
            gemini (read-heavy sweeps, research); omitted = per task, claude for the rest
  profile=  vault profile that bills the codex/gemini leaves (dev, prod, …); omitted = the session profile
  mode      auto (default: parallel if independent, sequential if coupled or from a plan) · parallel ·
            sequential (spec → simplify → quality gate per task); team mode moved to /coworker team
  no task   runs the newest plan's task list (~/.claude/plans/…)

Feature and bug-fix tasks also get a copilot test agent in the same message.

Examples:
  /code-worker fix the watchdog flake | rename ResolveEnv to CanonicalizeEnv   auto, two leaves
  /code-worker sequential                                                     newest plan, gated per task
  /code-worker agent=codex review the diff on this branch                       codex leaf
  /code-worker profile=prod agent=gemini scan internal/memory for dead exports  gemini billed to prod
```
