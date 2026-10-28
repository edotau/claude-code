---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Orchestration

## Available Agents

Located flat at `agents/<name>.md`. Agents hold no per-agent memory scope; they only PROPOSE
memory in their reports. The main session writes it to Claude Code's native project memory,
`~/.claude/projects/<slug>/memory/MEMORY.md` (a lean index) plus the topic files beside it.

`agents/<name>.md` is the single definition, on one skeleton —
`## Role · ## Owns · ## Verbs · ## Hands off to · ## Procedure · ## Report contract · ## Don't`;
`commands/<name>.md` is a ~10-line launcher (how to read `$ARGUMENTS`, which agent/script to run,
relay the report, a pointer to the agent file); shell logic a skill needs lives in
`skills/<name>/scripts/`, anything else in a Go verb or a Makefile target.

Roster: the files in `agents/` (`claude-code agents` lists the CLI agents, not this roster). Each agent's
purpose and when to use it is its `description:` frontmatter in `agents/<name>.md`.

## Codebase Intelligence Protocol

Trigger, agent table and skip condition: `rules/workflow/master-workflow.md` → Phase 2 (also runnable
as orchestrate `--scan`, `subagent-workflows`). Model assignment this doc adds: Structure/Conventions
run on Sonnet (`Explore`); when the area is large, the Dependencies row runs on Gemini
(`claude-code gemini bridge`) instead — its 1M-token window covers a wide sweep in one call.

## Immediate Agent Usage

No user prompt needed — trigger automatically:

1. Non-trivial task received — run the **Codebase Intelligence Protocol** first
2. Complex feature requests — use the **Plan** agent (design; feed it the context brief), then **claude** builds
3. Code just written/modified — use **claude** agent (review phase)
4. Architectural decision — use the **Plan** agent (design)

Test-agent dispatch is not on this list by itself: `../standards/quality.md` → "every code dispatch carries a
test agent, concurrently" is the one rule, triggered by every item above that changes code.

## Parallel Task Execution

ALWAYS use parallel Agent calls for independent operations — launch in a **single message**.

- **At most 24 concurrent Agent dispatches machine-wide, test agents included** (12 lanes × implementer + test
  agent) — the `agent-inflight` hook enforces it (`internal/hooks/inflight.go`).
- **Cost per leaf is controlled by the leaf's own `model:` frontmatter, not by the session.**
  An `inherit`-model agent (`claude`, `codex`) dispatched *as a fan-out leaf* runs at the
  parent's model — dispatch fan-out leaves as `code-worker` (frontmatter `model: sonnet`),
  NOT as `inherit` managers (Opus only by escalation — below).
- A provider with its own rate limit may throttle a wide fan-out before either cap bites —
  `claude-code doctor` reports profile/proxy health; drop to sequential if a shared
  provider starts throwing sustained errors.

```markdown
# GOOD: Single message, 3 parallel agents
Agent 1: Security analysis of auth module
Agent 2: Performance review of cache system
Agent 3: Type checking of utilities

# BAD: Sequential when unnecessary
First agent 1, then agent 2, then agent 3
```

### Escalation & fallback

- Leaves never auto-fallback — a leaf runs the `model:` it was dispatched with, success or fail.
- On capability shortfall, the **manager** re-dispatches the leaf with `model: 'opus'` (counted
  against the ≤24 cap). Escalate after ONE failed high-effort Sonnet attempt —
  repeat Sonnet retries on a capability-bound task are pure token burn.
- Read-only sweeps that failed cheaply may be re-dispatched as `haiku` instead.
- Managers tune `effort` per dispatch: low for mechanical leaves, medium default, high only
  for a single gnarly task — prefer dropping effort before dropping a model.
- Inherit-model managers never pin effort — they run at the session's effort.
- Agent choice and fallback: `claude-code gateway router` (skills/subagent-workflows/references/cost-routing.md →
  Fallback).

## Cross-Agent Teams

Managers (top-level claude, a Workflow, the main session) may recruit codex / copilot / gemini as extra hands via
`claude-code ask --agent <agent>`; a leaf runs the agent its brief names the same way, and recruits no other.

Route by use case: `../../skills/subagent-workflows/references/cost-routing.md`.

### Parallel vs sequential

- **Parallel — the default when work is independent.** Push wide when tasks don't depend on
  each other, especially across distinct providers (N independent module passes; a codex
  adversarial dispatch alongside a Claude pass).
- **Sequential — only when forced.** One dispatch's output feeds the next, they touch shared state,
  or a shared provider is throwing sustained errors. The writer→verifier and migrate→verify
  splits are **sequential by nature** — running them at once just races the same files.

## Extended thinking and prompt caching (authoring)

Current Claude models run thinking as **adaptive** — the SDK omits a fixed `budget_tokens` unless the specific
model requires one. Don't hand-roll a thinking-budget override in a skill or command; a model that genuinely
needs a fixed budget gets it in provider config, not in prose repeated per task.

For repeated large prefixes (system prompts, CLAUDE.md, big tool schemas), mark the stable prefix with
`cache_control` so re-sends bill at the cached rate. Content that changes every call must not sit inside a
cached block, or the cache never hits or silently serves stale context.
