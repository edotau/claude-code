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

| Agent | Purpose | When to Use |
|-------|---------|-------------|
| Explore (built-in) | Fast codebase scanning | **First step** — before planning or coding |
| Plan | Read-only planner; overrides the built-in, preloads the `planning` skill, ends every plan with `## Tasks` | Plan mode, plan-only work, MEDIUM+ before code |
| claude | Full-lifecycle coding: architect → plan → implement → simplify → review (incl. plan-only path) | Architectural decisions, MEDIUM+ changes, plan writing, post-write review |
| code-worker | Sonnet-tier fan-out leaf: one well-scoped task in-context, effort per dispatch | Dispatched by a manager (claude / Workflow / main session) — never ambient |
| copilot | The test surface (repairs EXISTING tests a change breaks or makes stale, writes NEW coverage after the live check) + last mile: merge branches in dependency order, then stage + commit | Renames/signature changes, a suite that was green before the change, new behavior once the live check confirms it (dispatch WITH the implementer, not after); merge/stage/commit after the gates are green |
| codex | Adversarial REVIEW (pre-impl task/diff vetting) · VERIFY (format→lint→test→security gate loop) · OPTIMIZE (harness tuning) · SKILL/PLUGIN (artifact validation) | Before implementing; before shipping a PR; harness cost/reliability; reviewing an authored skill or plugin |
| gemini | Codebase scanning on agy (Google sign-in): `ask --agent gemini --tools` or `gemini bridge` for 1M-token sweeps | Architecture, refactor impact, cross-file surveys |
| opencode | DOCS refresh/lookup · ANALYZE native project memory · proven-pattern EXTRACT · session PERSIST | Updating docs, API reference; `/reflection:*` — auditing/extracting/persisting memory |

## Codebase Intelligence Protocol

**Recommended first step** for any non-trivial task. Run `/workflow` or launch the scan manually
before engaging any planning or implementation agent.

The protocol launches **2-3 parallel agents** in a **single message**, scoped to the area the
task touches:

| Agent | Model | Focus | Output |
|-------|-------|-------|--------|
| Structure | Sonnet (Explore) | Directory layout, entry points, pipeline stages | Package map + architecture overview |
| Conventions | Sonnet (Explore) | Naming, error handling, logging, test patterns | Style rules + code references |
| Dependencies / wide sweep | Gemini (`claude-code gemini bridge`) | Manifest deps, env vars, CLI tools, policies — when the area is large | Integration map + constraints |

Results are merged into a **Codebase Context Brief** — a structured summary passed as context to
every downstream agent so they operate with full codebase awareness.

**When to skip**: Single-line fixes, typo corrections, or tasks where context is already warm
from a recent scan in the same session. **Scope the fan-out** to the directories the task
touches — a repo-wide Explore sweep on every task is the single biggest avoidable input-token
cost of this protocol.

> For the full step-by-step workflow, see `commands/workflow/workflow.md`.

## Immediate Agent Usage

No user prompt needed — trigger automatically:

1. Non-trivial task received — run the **Codebase Intelligence Protocol** first
2. Complex feature requests — use **claude** agent (architect/plan phase; feed it the context brief)
3. Code just written/modified — use **claude** agent (review phase)
4. Bug fix or new feature — new coverage is **copilot**'s job, dispatched with the implementer, written after the live check
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

> **Tier by context weight, not raw count.**
> - **Deep-context agents** (large context / many files each — e.g. whole-module audits): keep
>   **≤8** concurrent.
> - **Lean/fresh subagents** (small context — one-file reads, single-target fixes): up to
>   **~8–10** is fine; they start cheap.
> - **Cost per leaf is controlled by the leaf's own `model:` frontmatter, not by the session.**
>   An `inherit`-model agent (`claude`, `codex`) dispatched *as a fan-out leaf* runs at the
>   parent's tier — dispatch fan-out leaves as `code-worker` (frontmatter `model: sonnet`),
>   NOT as `inherit` managers. Reserve `model: 'opus'` for a single leaf that demonstrably
>   failed on Sonnet (capability escalation), counted against the ≤8 tier.
> - A provider with its own rate limit may throttle a wide fan-out before either cap bites —
>   `claude-code doctor` reports provider/router health; drop to sequential if a shared
>   provider starts throwing sustained errors.

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
- **Adversarial / second-opinion review** → `codex` (`/codex:adversarial-review`) — a
  genuinely different model family catches what a same-model self-review won't.
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
