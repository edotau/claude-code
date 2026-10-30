---
name: subagent-workflows
description: Choose and run multi-agent work by task complexity — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, competition tournaments, multi-stage orchestration, and cheaper agent dispatches via `claude-code ask` — and the one handoff contract (brief in, report out, ownership, status). Use for "fan this out", "dispatch agents", "write a brief", "which agent". Leaves run `problem-solving`, `evaluate-code` or `simplify`.
---

# Subagent Workflows

Use the smallest shape that fits; add orchestration only when the dependency shape needs it.

| Shape | Tier | Do | Depth |
|---|---|---|---|
| A one-line edit | TRIVIAL | Inline; anything bigger is dispatched (CLAUDE.md guideline 8) | — |
| Independent tasks, disjoint files | HIGH, or any tier with ≥2 independent tasks | `Agent` calls in one message, `code-worker` ‖ `copilot` | [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md); scripted: `Workflow code-engineer` |
| Ordered plan, review gate per task | MEDIUM | One implementer at a time, spec → simplify → quality | [Sequential plan](#sequential-plan) |
| Review a diff | any | One read-only `evaluate-code` leaf (quality + security lenses), or two on a large diff — what `/code-review` runs; findings go back to an implementer running `problem-solving` (`../problem-solving/references/feedback.md`) | [prompts.md#code-quality-reviewer](references/prompts.md#code-quality-reviewer) |
| Many leaf tasks across packages | HIGH/EPIC | One Claude lane orchestrator per lane, agent leaves via `claude-code ask` | [lane-orchestrator.md](references/lane-orchestrator.md) |
| One objective, several phases and owners | HIGH/EPIC | explore → plan → implement → simplify → review → verify | [orchestrate.md](references/orchestrate.md) |
| Same measurable task, several approaches | any | N isolated attempts, re-measure, merge one | [competition-tournament.md](references/competition-tournament.md) |
| Cheaper or second-agent dispatch (Copilot, Codex, Gemini, OpenCode) | any | `claude-code ask --agent <agent>` | [cost-routing.md](references/cost-routing.md) |
| Repeatable pipeline, evaluator or bounded loop | any | A saved `Workflow` (`workflows/*.js`) — **only when the user asks for one** | [saved-workflows.md](references/saved-workflows.md) |

Tier is `rules/workflow/master-workflow.md` → Phase 0. Each leaf runs one method skill, named in the brief's `Method:`
field: `problem-solving` (`code-worker`, lane leaf), `evaluate-code` (spec, quality and plan reviewers;
codex review, vet and judge), `simplify` (simplifier).
Brief templates for implementer, spec, quality and plan reviewers: [prompts.md](references/prompts.md).
Shared discipline and per-agent memory-proposal candidates: [memory-updates.md](references/memory-updates.md).
Concurrency caps: `rules/workflow/agents.md` → Parallel Task Execution. Scoping a leaf below the repo root hides
`.git`/`.venv`: [grant-depth-trap.md](references/grant-depth-trap.md).

## Leaf types

A leaf either does the work itself or requests one agent through `claude-code ask --agent <x>`. The UI row is
`<leaf type>  <requested agent>: <task>` for a request, `<leaf type>  <task>` otherwise.

| Leaf type | Does the work itself | Requests |
| --- | --- | --- |
| `claude` — near the main thread | `claude  <task>` | rare: `claude  copilot: <task>` |
| `code-worker` — default fan-out leaf | `code-worker  <task>` | `code-worker  claude: reviewing code` |
| `codex` / `copilot` / `gemini` / `opencode` | `copilot  <task>` | `copilot  gemini: scanning repo` |
| `coworker` — always requests | — | `coworker  codex: <task>` |

"Terminal" means no subagents and no Workflow; an `ask` request is a subprocess call and is allowed whenever the brief
names it.

## Handoff contract

The one definition of what passes manager → leaf → manager. Agent files point here; the brief templates in
[prompts.md](references/prompts.md) repeat the fields literally because an agent CLI starts cold.

**Brief (in)** — a fresh agent must be able to act from the handoff alone:

- `Objective:` one outcome. `Agent:` the agent this leaf requests via `claude-code ask --agent <x>`
  ([cost-routing.md](references/cost-routing.md)); omitted, `in-context`, or the leaf's own type = does the work
  itself. One line; `Mode:` decides, and several names with no `Mode:` = relay (`gemini → codex`).
  The hook turns it into the label ([Leaf types](#leaf-types)). `Agent:` is the only spelling.
- `Task:` (optional) the plan task id the leaf lands (`Task: 1.2`); the dispatch hook claims it in the plan.
- `Owns:` the exact files, plus "Do NOT edit <others> — agents are in this package concurrently".
- `Method:` the skill to follow, as a file path for a leaf without the `Skill` tool.
- `Workflow:` (optional) a saved Workflow the leaf may run, with its args — the only way a leaf starts one
  ([saved-workflows.md](references/saved-workflows.md) → Run from a leaf).
- `Context:` paths with line numbers, prior decisions, the upstream artifact (stage doc, `--attach` lane plan,
  plan path).
- `Constraints:` the CLAUDE.md rules that apply, plus the shared-tree line below.
- `Acceptance:` an observable outcome. `Verify:` the scoped command and live check — never the full suite
  (`rules/standards/quality.md` → Verification Order).
- A code-changing leaf also gets the forced `TASK:`/`ISSUE:` lines verbatim — action-point lines transfer only in the
  prompt.
- `Return:` the report below. Size the model to the task (Sonnet leaf by default; raise effort before model).
- Before a cross-session `SendMessage`, run `claude-code agents tree --running` and pick the recipient by cwd +
  task, not by name.

**Report (out)** — lead with the outcome: the verdict word for a review, the status word for a change, the findings
for a read-only research dispatch. Verdict words come first and alone, one closed vocabulary per mode
(`evaluate-code` → Verdicts); gate loop `PASS | FAIL` (`problem-solving/references/verify.md`); spec review
`SPEC_COMPLIANT | SPEC_ISSUES` ([prompts.md#spec-reviewer](references/prompts.md#spec-reviewer)); a change reports the
Status below.

- Status: `DONE` | `DONE_WITH_CONCERNS` | `NEEDS_CONTEXT` | `BLOCKED`.
- `Ran on:` quote the `ask: ran on …` line · job · session; `self — <why>` in-context. A stand-in is never hidden.
- Files changed, and the API surface the integrator wires.
- Verification: each command, its exit code, the load-bearing output.
- Coverage: each `Acceptance:` item → the evidence that settles it (file:line, command output, URL), or the gap left.
- `TASK:`/`ISSUE:`/`PENDING:` facts as plain sentences (`../problem-solving/references/notes.md`); surprises;
  anything found outside `Owns:`.
- Test agent: tests planned but not written. Optional: [proposed memory updates](references/memory-updates.md).

**Ownership**

- One writer per file. Same package parallelizes when each agent owns a new file and the manager alone makes the
  integration edit.
- The manager owns decisions, integration and final verification; commits are the manager's, a lane orchestrator's
  (pathspec), or `copilot` LAST-MILE's. Leaves never dispatch subagents, commit or run the suite; a leaf runs a saved
  Workflow only when its brief names one (`Workflow:`), headless per
  [saved-workflows.md](references/saved-workflows.md) → Run from a leaf.
- Before fanning out more, the manager checks the load with `claude-code agents tree --running`, follows an agent
  dispatch with `claude-code <agent> tail -f <job>`, and clears dead entries with `claude-code agents reap`.
- A feature or bug-fix dispatch carries a test agent: `rules/standards/testing-guidelines.md` → Test agent (it waits
  for "landed", never polls).

| Status | Manager action |
|---|---|
| `DONE` | Spec review |
| `DONE_WITH_CONCERNS` | Resolve correctness or scope concerns first |
| `NEEDS_CONTEXT` | Add the context and resume the same agent ([Return to owner](#return-to-owner)) |
| `BLOCKED` | Change something — clarify, split, raise capability, or revisit the plan; never resend unchanged |

## Delegation gate

Decide ownership before gathering evidence. Work that needs investigation, a change, or verification beyond what is
already in context and one bounded lookup goes to an owner; do not investigate in the manager to decide whether to
delegate — knowing the file names is not having the evidence. Finish inline only when the evidence is already in hand.
One connected outcome gets one owner even without a parallel branch; add owners only for independent deliverables or
a required independent check, never to split by file, question, or language alone. While an owner works, do only
distinct work — never repeat its searches.

## Return to owner

A gap, a missing `Acceptance:` item, or a correction goes back to the same agent (`SendMessage` resumes it with its
context): name the gap, the evidence wanted, and the stop condition; batch independent questions. It amends; it does
not restart the investigation, and no reviewer is added for it. Wait on completion notifications — elapsed time alone
is not a stall, and status polling is not progress. Re-dispatch fresh only when the owner is gone; then hand over its
established evidence.

## Rules

- Parallelize only independent work.
- **Simplify before review, every time code changed**: `Skill(simplify)` over the diff, or a leaf briefed with
  [prompts.md#simplifier](references/prompts.md#simplifier). Never an ad hoc "clean this up" brief.
- Advisory detectors (`claude-code review *`) run once per pipeline: the simplifier runs `complexity`+`diff`; the
  manager runs the rest and pastes the output into the quality reviewer's brief; no later stage re-runs them. The
  mechanical `make fmt && make lint` gate is separate and runs after every code change (`rules/standards/quality.md`
  → Verification Order).

## Sequential plan

Per task: `code-worker` ‖ `copilot`, same message → spec review until `SPEC_COMPLIANT`
([prompts.md#spec-reviewer](references/prompts.md#spec-reviewer)) → `Skill(simplify)` → quality review until
`SHIP` ([prompts.md#code-quality-reviewer](references/prompts.md#code-quality-reviewer)) → run the task's
verification yourself. Order is always spec → simplify → quality: simplifying non-compliant code polishes the
wrong thing, and reviewing before simplifying reviews code about to change. Never overlap implementers.

Stop and ask the user when an assumption breaks (missing file, changed API), scope grows, or a decision falls
outside the plan.

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
