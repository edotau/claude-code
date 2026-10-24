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

| Agent | Purpose | When to Use |
|-------|---------|-------------|
| Explore (built-in) | Fast codebase scanning | **First step** — before planning or coding |
| Plan | Read-only planner; owns design and the plan template (`problem-solving` Steps 0-3), ends every plan with `## Tasks` | Plan mode, plan-only work, MEDIUM+ before code |
| claude | Builds and manages: implement → simplify → hand review to evaluate-code; top-level orchestration | MEDIUM+ changes, multi-task plans to execute, post-write simplify |
| code-worker | Sonnet-tier leaf: one well-scoped task in-context (`problem-solving` Steps 4-6), effort per dispatch, or via a vendor runner named by `Runner:` (copilot/codex/gemini/opencode) | Dispatched by a manager (claude / Workflow / main session) — never ambient |
| copilot | The test surface (repairs EXISTING tests a change breaks or makes stale, writes NEW coverage after the live check) + last mile: merge branches in dependency order, then stage + commit; every `claude-code copilot <verb>` (`ask --tools` for delegated mechanical work, `review`, `resume`, `status|result|cancel`) | Renames/signature changes, a suite that was green before the change, new behavior once the live check confirms it (dispatch WITH the implementer, not after); merge/stage/commit after the gates are green |
| codex | General-purpose agent on the Codex CLI (`claude-code codex <verb>`): `review [--adversarial]` on a diff · `ask` vets a task brief, runs the final gate chain (`--tools`), audits a skill/plugin, or takes any delegated task · `transfer` hands a session over | Second opinion on a diff or plan; the final gate pass before a PR; a skill or plugin to review; work to delegate to another model family |
| gemini | Explore alternative on agy (Google sign-in): `claude-code gemini explore` (code scan + web research brief), `gemini ask --tools`, `gemini bridge` for 1M-token sweeps, `gemini review [--adversarial]` | Exploring before a plan, research and prior art, architecture, refactor impact, cross-file surveys, web lookups |
| opencode | DOCS refresh/lookup on the OpenCode CLI (`claude-code opencode ask --tools`) · ANALYZE native project memory (`memory review`) · proven-pattern EXTRACT (`memory promote --to skill`) · session PERSIST (`memory update`) · `opencode review|resume` | Updating docs, API reference; `claude-code memory <verb>` — auditing/promoting/persisting memory |

## Codebase Intelligence Protocol

Trigger, agent table and skip condition: `rules/workflow/master-workflow.md` → Phase 2 (also runnable
as orchestrate `--scan`, `subagent-workflows`). Model assignment this doc adds: Structure/Conventions
run on Sonnet (`Explore`); when the area is large, the Dependencies row runs on Gemini
(`claude-code gemini bridge`) instead — its 1M-token window covers a wide sweep in one call.

## Immediate Agent Usage

No user prompt needed — trigger automatically:

1. Non-trivial task received — run the **Codebase Intelligence Protocol** first
2. Complex feature requests — use **claude** agent (architect/plan phase; feed it the context brief)
3. Code just written/modified — use **claude** agent (review phase)
4. Bug fix or new feature — new coverage is **copilot**'s job, dispatched with the implementer, written after the live
   check
5. Architectural decision — use **claude** agent (architect phase)
6. **Implementation starting** — dispatch the test-surface agent in the SAME message as the
   first implementation step, never after the code lands. It plans the additions AND finds
   the existing tests the change breaks, concurrently. Canonical rule: `standards/quality.md`
   → "every code dispatch carries a test agent, concurrently". It writes tests AFTER the live
   check of the change, sized to the change, and runs only the touched packages' tests
   ("Live first, then tests").

## Parallel Task Execution

ALWAYS use parallel Agent calls for independent operations — launch in a **single message**:

> **Hard minimum on any code dispatch: ≥1 test agent in the same message as the implementation
> agent(s).** Not queued after them. Ownership split, sizing, and what does *not* satisfy it:
> `../standards/quality.md` → "every code dispatch carries a test agent, concurrently".
>
> **Tier by context weight, not raw count.**

- **Deep-context agents** (large context / many files each — e.g. whole-module audits): keep
  **≤8** concurrent.
- **Lean/fresh subagents** (small context — one-file reads, single-target fixes): up to
  **~8–10** is fine; they start cheap.
- **Cost per leaf is controlled by the leaf's own `model:` frontmatter, not by the session.**
  An `inherit`-model agent (`claude`, `codex`) dispatched *as a fan-out leaf* runs at the
  parent's tier — dispatch fan-out leaves as `code-worker` (frontmatter `model: sonnet`),
  NOT as `inherit` managers. Reserve `model: 'opus'` for a single leaf that demonstrably
  failed on Sonnet (capability escalation), counted against the ≤8 tier.
- A provider with its own rate limit may throttle a wide fan-out before either cap bites —
  `claude-code doctor` reports provider/router health; drop to sequential if a shared
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
  against the ≤8 deep-context tier). Escalate after ONE failed high-effort Sonnet attempt —
  repeat Sonnet retries on a capability-bound task are pure token burn.
- Read-only sweeps that failed cheaply may be re-dispatched as `haiku` instead.
- Managers tune `effort` per dispatch: low for mechanical leaves, medium default, high only
  for a single gnarly task — prefer dropping effort before dropping a tier.
- Inherit-model managers never pin effort — they run at the session's effort.

## Cross-Agent Teams

Managers (top-level claude, a Workflow, the main session — **never a leaf**) may recruit
codex / copilot / gemini as extra hands via `claude-code ask --agent <runner>`.

### Recruit + route by use case

- **Quick edits / mechanical changes / search** → `haiku` or `sonnet` leaves (cheap, fast).
  Prefer a Claude sonnet/haiku leaf over recruiting a whole external agent for small work.
- **Long-context exploration** → `gemini` (its window is the reason to use it).
- **Adversarial / second-opinion review** → `codex` (`/codex`, `claude-code codex review
  --adversarial`) — a genuinely different model family catches what a same-model self-review won't.
- **Heavy reasoning / architecture** → keep it on `opus`.

### Parallel vs sequential

- **Parallel — the default when work is independent.** Push wide when tasks don't depend on
  each other, especially across distinct providers (N independent module passes; a codex
  adversarial leg alongside a Claude pass).
- **Sequential — only when forced.** One leg's output feeds the next, they touch shared state,
  or a shared provider is throwing sustained errors. The writer→verifier and migrate→verify
  splits are **sequential by nature** — running them at once just races the same files.

## Multi-Perspective Analysis

For complex problems, use split-role sub-agents:

- Factual reviewer
- Senior engineer
- Security expert
- Consistency reviewer
- Redundancy checker

## Extended thinking and prompt caching (authoring)

Current Claude models run thinking as **adaptive** — the SDK omits a fixed `budget_tokens` unless the specific
model requires one. Don't hand-roll a thinking-budget override in a skill or command; a model that genuinely
needs a fixed budget gets it in provider config, not in prose repeated per task.

For repeated large prefixes (system prompts, CLAUDE.md, big tool schemas), mark the stable prefix with
`cache_control` so re-sends bill at the cached rate. Content that changes every call must not sit inside a
cached block, or the cache never hits or silently serves stale context.
