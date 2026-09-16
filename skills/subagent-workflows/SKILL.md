---
name: subagent-workflows
description: Choose and run concise multi-agent workflows for independent parallel tasks, sequential plan execution with review gates, structured orchestration, competition tournaments, or cross-model teams via `claude-code ask`.
---


# Subagent Workflows

Use the smallest workflow that matches the task:

| Work shape | Action |
|---|---|
| One small task | Work inline |
| Independent tasks | Read [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md) |
| Ordered plan with review gates | Read [subagent-driven-development.md](references/subagent-driven-development.md) |
| Pipeline, router, evaluator, or dynamic orchestration | Read [choosing-a-pattern.md](references/choosing-a-pattern.md) |
| A second model family adds useful coverage | Read [cross-model-agent-teams.md](references/cross-model-agent-teams.md) |
| Several agents should attempt the same measurable task and the best result wins | Read [competition-tournament.md](references/competition-tournament.md) |
| One objective spans several phases with different owners and skills | Run `/workflow:orchestrate` |

Model tiers: `claude-code models` shows what each slot (opus/sonnet/haiku) resolves to. Lean leaves
(Sonnet) ~8–10 in flight; deep-context (Opus) agents ≤4.

## Dispatch contract

Give every agent:

- one objective and bounded scope;
- relevant constraints and artifacts;
- acceptance criteria and an exact verification command;
- expected output;
- model/effort sized to the task (Sonnet leaf by default; raise effort before model).

A fresh agent must be able to act from the handoff alone.

## Rules

- Parallelize only independent work. Sequence dependencies and shared writes.
- Give each mutating agent exclusive file ownership. Same PACKAGE parallelizes when each agent owns a
  new file and the manager alone does the integration edit — see dispatching-parallel-agents.md.
- Prefer new-file ownership over `isolation: "worktree"` when the tree allows it; worktrees add
  setup cost and can vanish mid-run.
- Keep the manager responsible for decisions, integration, and final verification.
- Treat agent reports as claims: inspect outputs and run the integrated checks yourself. A test that
  has never been watched to fail is not yet a guard.
- Dispatch a test agent in the SAME message as the implementers (`rules/standards/quality.md`).
- Use `scripts/workflow_skeleton.py` only for repeatable `Workflow` structures.

## In this repo

`~/.claude` is ONE tree shared by subagents, other live sessions, and harness jobs. Every brief must
carry: never `git stash`/`checkout`/`reset`/`restore` (CLAUDE.md hard rules), gate on a SCOPED
target, and confirm each write survived with `git diff --stat`. Details in
[dispatching-parallel-agents.md](references/dispatching-parallel-agents.md).
