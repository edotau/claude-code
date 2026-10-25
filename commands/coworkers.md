---
description: "EXPERIMENTAL — fan out coworker leaves that run each task on vendor CLIs (codex, copilot, gemini, opencode): single, compare or relay. The vendor counterpart of /code-workers."
argument-hint: "[single | compare | relay] [runners=gemini,codex,...] <task1 | task2 | ...>  (tasks '|'-separated)"
allowed-tools: Bash, Read, Grep, Glob, Agent
---

Launcher only. The leaf's procedure and report live in `agents/coworker.md`; the dispatch shape and the
Handoff contract in `skills/subagent-workflows/SKILL.md`.

From `$ARGUMENTS`, strip an optional leading mode word (`single`/`compare`/`relay`; default `single`) and
an optional `runners=` list (default: pick per task from `skills/subagent-workflows/references/cost-routing.md`
— gemini for sweeps/research, codex for review, opencode for edits, copilot for mechanical work); split the rest
on `|` into the task list.

Dispatch each task as one `coworker` leaf (`subagent_type: "coworker"`), all in ONE message in the background,
with a brief per the Handoff contract that names `Runners:`, `Mode:`, `Owns:` and `Verify:`. A task that
changes code also gets a `copilot` test agent in the SAME message (`rules/standards/quality.md`) and runs its
edits in a worktree. Relay one consolidated report: per task, the runner rows (runner · model · job · fallback ·
verified), the verified answer or change, and for `compare` where the runners agreed and differed.

Leg choice and fallback: `claude-code route` (skills/subagent-workflows/references/cost-routing.md → Fallback).
