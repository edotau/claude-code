---
description: "Codebase intelligence scan — parallel Explore/Gemini agents → Context Brief → optional plan"
argument-hint: "[<task description to chain into plan mode>]"
---

# /workflow — Codebase Intelligence Scan

Runs the **Codebase Intelligence Protocol** (canonical definition: `rules/workflow/agents.md` →
"Codebase Intelligence Protocol"). This command is the operational trigger; the protocol doc is
the source of truth — don't restate it here.

**Input**: `$ARGUMENTS` (a task description, or blank for a standalone scan)

## Do

1. Launch **2-3 agents in a single message** (parallel), scoped to the area the task touches
   rather than the whole tree:
   - **Structure** — Explore (Sonnet): directory layout, entry points, pipeline stages.
   - **Conventions** — Explore (Sonnet): naming, error handling, logging, test patterns.
   - **Dependencies / wide sweep** — `claude-code gemini bridge --dirs <d> "<task>"` for a
     1M-context pass when the area is large (manifest deps, env vars, CLI tools, cross-file
     policies).
   Merge results into a **Codebase Context Brief** (structure map + conventions + integration map).
2. **If `$ARGUMENTS` names a task**: chain into plan mode with the brief as context.
   Otherwise: print the brief and stop.

## Notes

- Skip for single-line/typo fixes or when a scan is already warm this session.
- Scope the fan-out to the area the task actually touches — a repo-wide Explore sweep on every
  invocation is the biggest avoidable input-token cost; narrow `--dirs`/paths before fanning out.
- The full fan-out is a deep-context dispatch (see the concurrency tiers in
  `rules/workflow/agents.md`) — keep it to 2-3 agents, not a wide swarm.
