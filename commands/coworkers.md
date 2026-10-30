---
description: "Fan out coworker leaves that run each task on other agents (codex, copilot, gemini, opencode): single, compare or relay. The cross-agent counterpart of /code-workers."
argument-hint: "[single | compare | relay] [agents=gemini,codex,...] <task1 | task2 | ...>  (tasks '|'-separated)"
allowed-tools: Bash, Read, Grep, Glob, Agent
---

Launcher only. The leaf's procedure and report live in `agents/coworker.md`; the dispatch shape and the
Handoff contract in `skills/subagent-workflows/SKILL.md`.

From `$ARGUMENTS`, strip an optional leading mode word (`single`/`compare`/`relay`; default `single`) and
an optional `agents=` list (default: per `skills/subagent-workflows/references/cost-routing.md`); split the rest
on `|` into the task list.

Dispatch each task as one `coworker` leaf (`subagent_type: "coworker"`), all in ONE message in the background,
with a brief per the Handoff contract that names `Agent:` (one line), `Mode:` (`single|compare|relay`; absent with several names = relay), `Owns:` and `Verify:`. A task that
changes code also gets a `copilot` test agent in the SAME message (`rules/standards/quality.md`) and runs its
edits in a worktree. Relay one consolidated report: per task, the agent rows (agent · model · job · fallback ·
verified), the verified answer or change, and for `compare` where the agents agreed and differed.

Pick and fallback: `skills/subagent-workflows/references/cost-routing.md`.
