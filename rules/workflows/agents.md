---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Orchestration

## Commands

| To… | Run |
| --- | --- |
| Ask another agent (one shot) | `claude-code ask --agent <agent> "<task>"` (`--format json` = envelope) · `ask --log` · `ask --retry <id>` · inline agents: `ask --agents <json\|file>` (`rules/harness/agent.md` → Dispatch behavior) |
| Stream events, set env/system prompt, run remote, or hold a live claude session | `ask --format stream-json` · `--env K=V` · `--append-system-prompt` · `--host <h>` · `--live` + `agents send <id> "<task>"`; `agents status\|result\|tail\|cancel <id>`; `agents --format json` = capability schema (`cost-routing.md` → Ask flags and lifecycle) |
| Route to the cheapest capable agent | classify `sweep\|review\|mechanical\|standard\|reasoning`, then `ask --agent` or `gemini bridge` (`skills/subagent-workflows/references/cost-routing.md`) |
| Launch or drive codex/gemini/opencode/copilot | `claude-code <v> [args]` (`--` forces passthrough) · `<v> ask\|resume\|review\|transfer\|status\|result\|cancel\|setup\|catalog` · `--print-env` |
| Sweep many files in one Gemini call (1M context) | `claude-code gemini bridge --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"` · `gemini ask` drives agy |
| Fan out a MEDIUM+ change (MEDIUM only with 2+ parallel-safe tasks) | `/code-workers` leaves on disjoint packages, plus `copilot` only for features and bug fixes (`rules/standards/testing-guidelines.md`), same message (`master-workflow.md` → Phase 6) |
| Cross-agent leaves (compare, relay) | `/coworkers` |
| Dispatch on another profile | brief `Profile: <name>` → `coworker` runs `ask --agent <x> --profile <p>`; subagents and `agent()` stay on the session route |
| Run a saved Workflow — only when the user asks | `Workflow code-engineer\|code-analyzer\|green-keeper-loop\|code-agent` (`skills/subagent-workflows/references/saved-workflows.md`) |
| Review a diff; second opinion on another model | `/code-review` · `claude-code codex review [--adversarial] [--base <b>]` · `/codex` |
| Project agents + CLAUDE.md into every agent CLI | `claude-code agents sync [--agent a] [--dry-run\|--check]` (also run by `install`) |
| Subagent status | `claude-code agents tree [--watch\|--background]` · `agents reap` |

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Skeleton:
  `Role · Owns · Verbs · Hands off to · Procedure · Report contract · Don't` (`##` sections); `description:` says when
  to use it.
- `commands/<name>.md` is a ~10-line launcher: read `$ARGUMENTS`, run the agent/script, relay the report, point at the
  agent file. Skill shell logic lives in `skills/<name>/scripts/`; anything else in a Go verb or Makefile target.
- Agents hold no memory scope; they PROPOSE memory in their report and the main session writes it.
- `claude-code agents` lists the CLI agents, not this roster.

## When to dispatch without being asked

1. Non-trivial task — the Codebase Intelligence Protocol first (`master-workflow.md` → Phase 2):
   Structure/Conventions on `Explore` (Sonnet), Dependencies on `claude-code gemini bridge` when the area is large.
2. Complex feature or architectural decision — `Plan` designs, then `claude` builds.
3. Code just written — review per `../standards/quality.md` → Completion (`/code-review`); dispatch `claude` only
   when the review needs design judgment across modules.

Test agent: `../standards/testing-guidelines.md` → Test agent.

## Parallel dispatch

- **Independent work goes out in one message**; sequential only when one output feeds the next, they share state,
  or a shared provider throws sustained errors. Writer→verifier and migrate→verify are sequential by nature.
- Concurrency cap (12 per account): `../harness/hooks.md` → dispatch cap. A provider's own rate limit may bite first —
  `claude-code doctor` shows health; drop to sequential on sustained errors.
- **Leaf cost is the leaf's `model:` frontmatter, not the session.** An `inherit` agent (`claude`, `codex`)
  dispatched as a fan-out leaf runs at the parent's model — fan out as `code-worker` (`model: sonnet`).
- Leaf types, the `Agent:` request and its label: `../../skills/subagent-workflows/SKILL.md` → Leaf types; routing:
  `../../skills/subagent-workflows/references/cost-routing.md`.

## Escalation

- Leaves never auto-fallback. On a capability shortfall the **manager** re-dispatches with `model: 'opus'` after ONE
  failed high-effort Sonnet attempt — repeated Sonnet retries are token burn. A cheap read-only sweep may retry on
  `haiku`.
- Managers tune `effort` per dispatch (low mechanical, medium default, high for one gnarly task); drop effort before
  dropping a model. Inherit-model managers never pin effort.

## Authoring: thinking and caching

- Thinking is adaptive; never hand-roll a thinking budget in a skill or command — a model that needs one gets it in
  provider config.
- Mark stable large prefixes with `cache_control`; content that changes per call must stay outside the cached block.
