# Subagent-Driven Development

An ordered plan executed in this session, one task at a time with its reviews. Use
[dispatching-parallel-agents.md](dispatching-parallel-agents.md) when the tasks are truly independent.

## Setup

Confirm the plan, order, dependencies and acceptance criteria (review a written plan first with
[prompts.md#plan-reviewer](prompts.md#plan-reviewer)); record the base revision.

## Per task

1. Implementer ([prompts.md#implementer](prompts.md#implementer)) ‖ `copilot`, same message.
2. Spec review ([prompts.md#spec-reviewer](prompts.md#spec-reviewer)); send gaps back until `SPEC_COMPLIANT`.
3. Simplify the task diff (`Skill(simplify)`; its behavior gate is the task's verification command).
4. Quality review ([prompts.md#code-quality-reviewer](prompts.md#code-quality-reviewer)) with the simplify report;
   fix until `APPROVED`.
5. Run the task's verification yourself; mark it done.

Order is always spec → simplify → quality: simplifying non-compliant code polishes the wrong thing, and reviewing
before simplifying reviews code about to change. Never overlap implementers.

| Status | Action |
|---|---|
| `DONE` | Spec review |
| `DONE_WITH_CONCERNS` | Resolve correctness or scope concerns first |
| `NEEDS_CONTEXT` | Add the context, re-dispatch |
| `BLOCKED` | Change something — clarify, split, raise capability, or revisit the plan; never resend unchanged |

Stop and ask the user when an assumption breaks (missing file, changed API), scope grows, or a decision falls
outside the plan.

## Finish

`Skill(simplify)` over the integrated diff (cross-task duplication only shows here), review it against the whole plan,
then the repo's gates (`rules/standards/quality.md` → Verification Order). Hand off only with fresh evidence.
