# Subagent-Driven Development

Use this workflow for an ordered implementation plan that should stay in the current session. It is sequential: run one implementation task and its reviews before starting the next.

Use [dispatching-parallel-agents.md](dispatching-parallel-agents.md) instead when tasks are genuinely independent and safe to run concurrently.

## Setup

1. Confirm the plan, task order, dependencies, and acceptance criteria.
2. Record the base revision and working directory.
3. Prepare an isolated branch/worktree when the repository workflow requires it.

## Per-task loop

1. Dispatch one implementer using [implementer-prompt.md](implementer-prompt.md) and a `copilot` test agent
   in the SAME message (`rules/standards/quality.md`).
2. Inspect its status, changes, and test evidence.
3. Dispatch an independent spec reviewer using [spec-reviewer-prompt.md](spec-reviewer-prompt.md).
4. Send spec gaps back to the implementer; repeat until compliant.
5. Simplify the task diff with the `simplify` skill: `Skill(simplify)` in place, or a leaf briefed with
   [simplifier-prompt.md](simplifier-prompt.md). Its behavior gate is the task's verification command.
6. Only then dispatch a quality reviewer using [code-quality-reviewer-prompt.md](code-quality-reviewer-prompt.md),
   passing the simplify report.
7. Fix and re-review quality findings until approved.
8. Run the task verification and mark the task complete.

Never overlap implementers in this workflow. Gate order is always spec → simplify → quality: simplifying
non-compliant code polishes the wrong thing, and reviewing before simplifying reviews code about to change.

## Status handling

| Status | Action |
|---|---|
| `DONE` | Start spec review |
| `DONE_WITH_CONCERNS` | Resolve correctness or scope concerns before review |
| `NEEDS_CONTEXT` | Add the missing context and re-dispatch |
| `BLOCKED` | Change something: clarify, split the task, raise model capability, or revisit the plan |

Do not retry an unchanged prompt after a blocker.

## Finish

After all tasks:

1. Run `Skill(simplify)` over the integrated diff — cross-task duplication only shows here — then review it
   against the full plan.
2. Run repository-level format, lint, test, and security checks that apply.
3. Resolve every blocking finding and re-run failed checks.
4. Hand off only with fresh verification evidence.
