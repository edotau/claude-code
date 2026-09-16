# Subagent-Driven Development

Use this workflow for an ordered implementation plan that should stay in the current session. It is sequential: run one implementation task and its reviews before starting the next.

Use [dispatching-parallel-agents.md](dispatching-parallel-agents.md) instead when tasks are genuinely independent and safe to run concurrently.

## Setup

1. Confirm the plan, task order, dependencies, and acceptance criteria.
2. Record the base revision and working directory.
3. Prepare an isolated branch/worktree when the repository workflow requires it.

## Per-task loop

1. Dispatch one implementer using [implementer-prompt.md](implementer-prompt.md).
2. Inspect its status, changes, and test evidence.
3. Dispatch an independent spec reviewer using [spec-reviewer-prompt.md](spec-reviewer-prompt.md).
4. Send spec gaps back to the implementer; repeat until compliant.
5. Only then dispatch a quality reviewer using [code-quality-reviewer-prompt.md](code-quality-reviewer-prompt.md).
6. Fix and re-review quality findings until approved.
7. Run the task verification and mark the task complete.

Never overlap implementers in this workflow. Review order is always spec, then quality.

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

1. Review the integrated diff against the full plan.
2. Run repository-level format, lint, test, and security checks that apply.
3. Resolve every blocking finding and re-run failed checks.
4. Hand off only with fresh verification evidence.
