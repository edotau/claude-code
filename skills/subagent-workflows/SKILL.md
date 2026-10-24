---
name: subagent-workflows
description: Choose and run multi-agent work by task complexity — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, competition tournaments, multi-stage orchestration, and cheaper vendor legs via `claude-code ask` — and the one handoff contract (brief in, report out, ownership, status). Use for "fan this out", "dispatch agents", "write a brief", "which runner". Leaves run `problem-solving`, `evaluate-code` or `simplify`.
---

# Subagent Workflows

Use the smallest shape that fits; add orchestration only when the dependency shape needs it.

| Shape | Tier | Do | Depth |
|---|---|---|---|
| One small or tightly coupled task | TRIVIAL/LOW | Work inline | — |
| Independent tasks, disjoint files | HIGH, or any tier with ≥2 independent tasks | `Agent` calls in one message, `code-worker` ‖ `copilot` | [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md); scripted: `Workflow code-engineer` |
| Ordered plan, review gate per task | MEDIUM | One implementer at a time, spec → simplify → quality | [subagent-driven-development.md](references/subagent-driven-development.md) |
| Many leaf tasks across packages | HIGH/EPIC | One Claude lane orchestrator per lane, vendor leaves via `claude-code ask` | [lane-orchestrator.md](references/lane-orchestrator.md) |
| One objective, several phases and owners | HIGH/EPIC | explore → plan → implement → simplify → review → verify | [orchestrate.md](references/orchestrate.md) |
| Same measurable task, several approaches | any | N isolated attempts, re-measure, merge one | [competition-tournament.md](references/competition-tournament.md) |
| Cheaper or second-family leg (Copilot, Codex, Gemini, OpenCode) | any | `claude-code ask --agent <runner>` | [cost-routing.md](references/cost-routing.md) |
| Repeatable pipeline, evaluator or bounded loop | any | A saved `Workflow` (`workflows/*.js`) — **only when the user asks for one** | [saved-workflows.md](references/saved-workflows.md) |

Tier is `rules/workflow/master-workflow.md` → Phase 0. Each leaf runs one method skill, named in the brief's `Method:`
field: `problem-solving` (implementer, code-worker, lane leaf), `evaluate-code` (spec, quality and plan reviewers;
codex review, vet and judge), `simplify` (simplifier).
Brief templates for implementer, spec, quality and plan reviewers: [prompts.md](references/prompts.md).
Shared discipline and per-agent memory-proposal candidates: [memory-updates.md](references/memory-updates.md).
Concurrency: lean leaves ~8–10 in flight, deep-context agents ≤8 (`rules/workflow/agents.md`).

## Handoff contract

The one definition of what passes manager → leaf → manager. Agent files point here; the brief templates in
[prompts.md](references/prompts.md) repeat the fields literally because a vendor CLI starts cold.

**Brief (in)** — a fresh agent must be able to act from the handoff alone:

- `Objective:` one outcome. `Runner:` claude | copilot | codex | gemini | opencode
  ([cost-routing.md](references/cost-routing.md)).
- `Owns:` the exact files, plus "Do NOT edit <others> — agents are in this package concurrently".
- `Method:` the skill to follow, as a file path for a leaf without the `Skill` tool.
- `Context:` paths with line numbers, prior decisions, the upstream artifact (stage doc, `--attach` lane plan,
  plan path).
- `Constraints:` the CLAUDE.md rules that apply, plus the shared-tree line below.
- `Acceptance:` an observable outcome. `Verify:` the scoped command and live check — never the full suite (`make test`,
  `go test ./...`, bare `pytest`), which runs once at wrap-up with the user's approval.
- A code-changing leaf also gets the forced `TASK:`/`ISSUE:` lines verbatim — action-point lines transfer only in the
  prompt.
- `Return:` the report below. Size the model to the task (Sonnet leaf by default; raise effort before model).

**Report (out)** — lead with the outcome: the verdict word for a review (`evaluate-code` → Verdicts), the status
word for a change, the findings for a read-only research leg.

- Status: `DONE` | `DONE_WITH_CONCERNS` | `NEEDS_CONTEXT` | `BLOCKED`.
- `Ran on:` runner (model) · job · session · fallback <none|who> — a stand-in is reported, never hidden.
- Files changed, and the API surface the integrator wires.
- Verification: each command, its exit code, the load-bearing output.
- `TASK:`/`ISSUE:`/`PENDING:` facts as plain sentences (`../problem-solving/references/notes.md`); surprises;
  anything found outside `Owns:`.
- Test agent: tests planned but not written. Optional: [proposed memory updates](references/memory-updates.md).

**Ownership**

- One writer per file. Same package parallelizes when each agent owns a new file and the manager alone makes the
  integration edit.
- The manager owns decisions, integration and final verification; commits are the manager's, a lane orchestrator's
  (pathspec), or `copilot` LAST-MILE's. Leaves never dispatch, start a Workflow, commit or run the suite.
- Before fanning out more, the manager checks the load with `claude-code agents tree --running`, follows a vendor leg
  with `claude-code <vendor> tail -f <job>`, and clears dead entries with `claude-code agents reap`.
- Every code dispatch includes `copilot` in the same message; it waits for the manager's "landed" `SendMessage`
  (files + signatures), never a poll (`../testing-guidelines/references/test-discipline.md`).

| Status | Manager action |
|---|---|
| `DONE` | Spec review |
| `DONE_WITH_CONCERNS` | Resolve correctness or scope concerns first |
| `NEEDS_CONTEXT` | Add the context, re-dispatch |
| `BLOCKED` | Change something — clarify, split, raise capability, or revisit the plan; never resend unchanged |

## Rules

- Parallelize only independent work.
- **Simplify before review, every time code changed**: `Skill(simplify)` over the diff, or a leaf briefed with
  [prompts.md#simplifier](references/prompts.md#simplifier). Never an ad hoc "clean this up" brief.

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
