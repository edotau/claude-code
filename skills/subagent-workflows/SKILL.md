---
name: subagent-workflows
description: Choose and run multi-agent work — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, competition tournaments, multi-stage orchestration, and cheaper vendor legs via `claude-code ask`.
---

# Subagent Workflows

Use the smallest shape that fits; add orchestration only when the dependency shape needs it.

| Shape | Do | Depth |
|---|---|---|
| One small or tightly coupled task | Work inline | — |
| Independent tasks, disjoint files | `Agent` calls in one message, `code-worker` ‖ `copilot` | [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md) |
| Ordered plan, review gate per task | One implementer at a time, spec → simplify → quality | [subagent-driven-development.md](references/subagent-driven-development.md) |
| Many leaf tasks across packages | One Claude lane orchestrator per lane, vendor leaves via `claude-code ask` | [lane-orchestrator.md](references/lane-orchestrator.md) |
| One objective, several phases and owners | explore → plan → implement → simplify → review → verify | [orchestrate.md](references/orchestrate.md) |
| Same measurable task, several approaches | N isolated attempts, re-measure, merge one | [competition-tournament.md](references/competition-tournament.md) |
| Cheaper or second-family leg (Copilot, Codex, Gemini, OpenCode) | `claude-code ask --agent <runner>` | [cost-routing.md](references/cost-routing.md) |
| Repeatable pipeline, evaluator or bounded loop | A saved `Workflow` (`workflows/*.js`) — **only when the user asks for one** | `rules/workflow/master-workflow.md` → Saved Workflows |

Brief templates for implementer, spec, quality and plan reviewers: [prompts.md](references/prompts.md).
Concurrency: lean leaves ~8–10 in flight, deep-context agents ≤8 (`rules/workflow/agents.md`).

## Dispatch contract

Every brief carries: one objective; the exact files it owns (and "do NOT edit `<others>` — agents are in this package
concurrently"); constraints and artifacts; acceptance criteria; a verification command scoped to the touched
packages — never the full suite (`make test`, `go test ./...`, bare `pytest`), which runs once at wrap-up with the
user's approval; the expected report. Size the model to the task (Sonnet leaf by default; raise effort before model).
A fresh agent must be able to act from the brief alone.

## Rules

- Parallelize only independent work; one writer per file, always. Same package parallelizes when each agent owns a
  new file and the manager alone makes the integration edit.
- Every code dispatch includes `copilot` in the **same** message (`rules/standards/quality.md`); it waits for the
  manager's "landed" message, never a poll.
- **Simplify before review, every time code changed**: `Skill(simplify)` over the diff, or a leaf briefed with
  [simplify/references/leaf-brief.md](../simplify/references/leaf-brief.md). Never an ad hoc "clean this up" brief.
- The manager owns decisions, integration and final verification. Leaves never dispatch, commit or run the suite.

## Shared tree (`~/.claude` and any checkout several agents use)

Put in every brief: never `git stash` / `checkout` / `reset` / `restore` (CLAUDE.md hard rule 4); gate on a scoped
target (a repo-wide failure mid-fan-out is usually a sibling's half-finished edit — diff your own files, never stash
to check); confirm each write with `git diff --stat -- <file>` right after writing and again before reporting; stage
named files only. Prefer new-file ownership to `isolation: "worktree"` — worktrees add setup cost and have vanished
mid-run.

## Fan-in

Treat every report as a claim. Inspect the integrated diff for edits outside each agent's files; confirm a new guard
test can fail (break what it protects, re-run); re-run the scoped checks yourself (`-race` for concurrent code);
rebuild before trusting a binary; simplify the integrated diff (lanes duplicate helpers); report only verified results.
