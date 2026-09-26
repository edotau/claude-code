# Choosing a Pattern

Match the dependency shape; do not add orchestration by default.

| Shape | Pattern | Primitive |
|---|---|---|
| One task | Inline | No delegation |
| Independent tasks | Parallel fan-out | Multiple `Agent` calls in one message, or `Workflow.parallel()` |
| Each item passes through ordered stages | Pipeline | `Workflow.pipeline()` |
| Coupled plan tasks with review gates | Sequential review | One implementer and its reviewers at a time |
| Input selects one path | Router | Inline conditional, then dispatch |
| A planner discovers work and coordinates it | Orchestrator | Planning agent, then pipeline/fan-out |
| Output must pass a quality bar | Evaluator | Generate, independently verify, accept or revise |
| Discovery ends only when no new work appears | Bounded loop | Repeat with a limit and deduplicated `seen` set |
| Several approaches to one measurable task, best wins | Competition tournament | N leaves in worktrees, then rank and merge one — [competition-tournament.md](competition-tournament.md) |

Use ad hoc `Agent` calls when the manager should decide tasks during the session. Use `Workflow` when the structure must be repeatable. Generate a starting script with:

```bash
claude-code workflow skeleton pipeline|parallel|evaluator|orchestrator --name <name>
```

## Barrier rule

Prefer a pipeline. Add a barrier only when the next step needs the complete prior result set, such as deduplication, merging, or an empty-result exit.

Apply the dispatch and safety rules from [SKILL.md](../SKILL.md). Bound concurrency and loops.
