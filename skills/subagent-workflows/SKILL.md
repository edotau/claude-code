---
name: subagent-workflows
description: Choose and run concise multi-agent workflows for independent parallel tasks, sequential plan execution with simplify and review gates, structured orchestration, competition tournaments, or cross-model teams via `claude-code ask`.
---


# Subagent Workflows

Use the smallest workflow that matches the task:

| Work shape | Action |
|---|---|
| One small task | Work inline |
| Independent tasks | Read [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md) |
| Ordered plan with simplify + review gates | Read [subagent-driven-development.md](references/subagent-driven-development.md) |
| Pipeline, router, evaluator, or dynamic orchestration | Read [choosing-a-pattern.md](references/choosing-a-pattern.md) |
| Spend less Anthropic usage: send a step to Copilot/Codex/Gemini/OpenCode via `claude-code ask` | Read [cost-routing.md](references/cost-routing.md) |
| A second model family adds useful coverage | Read [cross-model-agent-teams.md](references/cross-model-agent-teams.md) |
| Many leaf-sized tasks on disjoint packages: Claude lane orchestrators fan out vendor leaves via `claude-code ask` | Read [lane-orchestrator.md](references/lane-orchestrator.md) |
| Several agents should attempt the same measurable task and the best result wins | Read [competition-tournament.md](references/competition-tournament.md) |
| One objective spans several phases with different owners and skills | Run `/workflow:orchestrate` |

Model tiers: `claude-code models` shows what each slot (opus/sonnet/haiku) resolves to. Lean leaves
(Sonnet) ~8–10 in flight; deep-context agents ≤8 (`rules/workflow/agents.md`).

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
- Dispatch `copilot` in the SAME message as the implementers (`rules/standards/quality.md`).
- **Simplify before review, every time code changed.** The `simplify` skill (`skills/simplify/SKILL.md`)
  is the only simplify procedure: the manager runs `Skill(simplify)` over the diff, or hands a leaf
  [simplifier-prompt.md](references/simplifier-prompt.md) (leaves read the skill as a file). Never an
  ad hoc "clean this up" brief; reviewers then judge the simplified diff.
- Use `claude-code workflow skeleton` only for repeatable `Workflow` structures.

## In this repo

`~/.claude` is ONE tree shared by subagents, other live sessions, and harness jobs. Every brief must
carry: never `git stash`/`checkout`/`reset`/`restore` (CLAUDE.md hard rules), gate on a SCOPED
target, and confirm each write survived with `git diff --stat`. Details in
[dispatching-parallel-agents.md](references/dispatching-parallel-agents.md).
