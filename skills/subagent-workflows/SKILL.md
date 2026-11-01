---
name: subagent-workflows
description: Choose and run multi-agent work by task complexity — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, competition tournaments, multi-stage orchestration, and cheaper agent dispatches via `claude-code ask` — and the one handoff contract (brief in, report out, ownership, status). Use for "fan this out", "dispatch agents", "write a brief", "which agent", "fix failing tests", "test-and-fix loop", "regression tests", "does this test have teeth", "pytest", "contract test". Leaves run `problem-solving`, `evaluate-code` or `simplify`.
---

# Subagent Workflows

Use the smallest shape that fits. This file is the one home for dispatch procedure; `CLAUDE.md` and `rules/` point here.

| Shape | Do |
|---|---|
| Inline | A one-line edit (TRIVIAL); anything bigger is dispatched |
| Parallel leaves, disjoint files | `Agent` calls in one message: `code-worker` (‖ `copilot` for a feature or bug fix, see [Test agent](#test-agent)); [dispatching-parallel-agents.md](references/dispatching-parallel-agents.md) |
| Sequential plan, a gate per task | [Sequential plan](#sequential-plan) |
| Saved `Workflow` | `workflows/*.js` — **only when the user asks**: [saved-workflows.md](references/saved-workflows.md) |

Also, one reference each: review a diff (`/code-review`, [prompts.md](references/prompts.md)) ·
[lane orchestrator](references/lane-orchestrator.md) · [orchestrate](references/orchestrate.md) ·
[tournament](references/competition-tournament.md) · [test-and-fix](references/test-and-fix.md) ·
[cost routing](references/cost-routing.md).

Tier is `rules/workflows/master-workflow.md` → Phase 0. Each leaf runs one method skill (`Method:`): `problem-solving`,
`evaluate-code` or `simplify`. Brief templates: [prompts.md](references/prompts.md). Scoping a leaf below the repo root
hides `.git`/`.venv` (`not a git repository`, mass `ModuleNotFoundError`): scope to the root; check with
`git rev-parse --show-toplevel` from the granted path.

## Leaf types

A leaf either does the work itself or requests one vendor through `claude-code ask --agent <x>` (a `coworker` leaf
always requests). Pass the task alone as `description`: the `dispatch-effort` hook prefixes the vendors the brief's
`Agent:` line names (minus the leaf's own type; `a → b` relay, `a, b` for `Mode: compare`) plus `·<profile>`, and
`code-worker` with none reads `claude`. Rows: `code-worker  claude: <task>`, `copilot  gemini: scanning repo`,
`coworker  claude·dev: <task>`. Never type the vendor name yourself.

"Terminal" means no subagents and no Workflow; an `ask` request is a subprocess call, allowed when the brief names it.
A leaf's cost is its `model:` frontmatter: an `inherit` agent (`claude`, `codex`) runs at the parent's model, so fan out
as `code-worker` (`model: sonnet`).

## Concurrency

At most 2 live leaves while other sessions share the checkout; otherwise the account cap in
`rules/harness/hooks.md` → dispatch cap (test agents count). Independent work goes out in one message; sequential only
when one output feeds the next or they share state, or on sustained provider errors (`claude-code doctor`).

## Handoff contract

What passes manager → leaf → manager. Agent files point here; [prompts.md](references/prompts.md) repeats the fields
literally because an agent CLI starts cold.

**Brief (in)** — a fresh agent must be able to act from the handoff alone:

- `Objective:` one outcome. `Agent:` the agent this leaf requests via `claude-code ask --agent <x>`
  ([cost-routing.md](references/cost-routing.md)); omitted, `in-context`, or the leaf's own type = does the work
  itself. One line; `Mode:` decides, and several names with no `Mode:` = relay (`gemini → codex`).
  `Agent:` is the only spelling ([Leaf types](#leaf-types)).
- `Profile:` (optional) a vault profile name: the hook moves the dispatch to `coworker` and the leaf runs
  `claude-code ask --agent <x> --profile <p>`; never a host or provider. Unknown names are denied; subagents and
  `agent()` stay on the session route.
- `Model:` (planned, not implemented in the hook): until then a leaf's model is its `model:` frontmatter.
- `Task:` (optional) the plan task id the leaf lands (`Task: 1.2`); the dispatch hook claims it in the plan.
- `Owns:` the exact files, plus "Do NOT edit <others> — agents are in this package concurrently".
- `Method:` the skill to follow, as a file path for a leaf without the `Skill` tool. `Return:` the report below.
- `Workflow:` (optional) a saved Workflow the leaf may run, with its args — the only way a leaf starts one
  ([saved-workflows.md](references/saved-workflows.md) → Run from a leaf).
- `Context:` paths with line numbers, prior decisions, the upstream artifact (stage doc, lane plan, plan path).
- `Constraints:` the CLAUDE.md rules that apply, plus the [Shared tree](#shared-tree) line.
- `Acceptance:` an observable outcome. `Verify:` the scoped command and live check, never the full suite
  (`rules/standards/quality.md` → Verification Order). A code-changing leaf gets the `TASK:`/`ISSUE:` lines verbatim.

**Report (out)** — lead with the outcome: the verdict word for a review (first and alone; `evaluate-code` → Verdicts,
`PASS | FAIL` for a gate loop, `SPEC_COMPLIANT | SPEC_ISSUES` per [prompts.md](references/prompts.md)),
the Status below for a change, the findings for read-only research.

- Status: `DONE` | `DONE_WITH_CONCERNS` | `NEEDS_CONTEXT` | `BLOCKED`.
- `Ran on:` quote the `ask: ran on …` line · job · session; `self — <why>` in-context. A stand-in is never hidden.
- Files changed and the API surface the integrator wires; verification (each command, exit code, load-bearing output).
- Coverage: each `Acceptance:` item → its evidence (file:line, output, URL) or the gap left.
- `TASK:`/`ISSUE:`/`PENDING:` facts as plain sentences (`../problem-solving/references/notes.md`); surprises; anything
  found outside `Owns:`; tests planned but not written; optional [memory proposals](references/memory-updates.md).

**Ownership**

- One writer per file. Same package parallelizes when each agent owns a new file and the manager alone makes the
  integration edit.
- The manager owns decisions, integration and final verification; commits are the manager's, a lane orchestrator's
  (pathspec), or `copilot` LAST-MILE's. Leaves never dispatch subagents, commit or run the suite; a leaf runs a saved
  Workflow only when its brief names one (`Workflow:`), headless per
  [saved-workflows.md](references/saved-workflows.md) → Run from a leaf.
- Check load with `claude-code agents tree`, follow a dispatch with `claude-code <agent> tail -f <job>`, clear dead
  entries with `claude-code agents reap`.

| Status | Manager action |
|---|---|
| `DONE` | Spec review |
| `DONE_WITH_CONCERNS` | Resolve correctness or scope concerns first |
| `NEEDS_CONTEXT` | Add the context and resume the same agent ([Return to owner](#return-to-owner)) |
| `BLOCKED` | Change something — clarify, split, raise capability, or revisit the plan; never resend unchanged |

## Delegation gate

Decide ownership before gathering evidence: work needing investigation, a change, or verification beyond what is in
context and one bounded lookup goes to an owner; finish inline only when the evidence is already in hand. One connected
outcome gets one owner; add owners only for independent deliverables or a required independent check. While an owner
works, do only distinct work — never repeat its searches.

## Return to owner

A gap or missing `Acceptance:` item goes back to the same agent (`SendMessage` resumes it with its context): name the
gap, the evidence wanted, and the stop condition; batch questions. It amends, not restarts, and adds no reviewer. Wait
on completion notifications; polling is not progress. A leaf's own long `claude-code ask` is the exception: it runs
`--background` and polls (`references/agent-verbs.md` step 3), since a foreground call idles it at 180s.
Re-dispatch fresh only when the owner is gone; then hand over its established evidence.
Never re-steer a running agent, except a `SendMessage` correction of an owner-changed requirement; pick a
cross-session recipient from `claude-code agents tree` by cwd and task, not by name.

## Rules

- **Simplify once per diff, before review**: the manager runs `Skill(simplify)`, or a leaf briefed with
  [prompts.md#simplifier](references/prompts.md#simplifier). Implementers never simplify their own diff — the
  `review-simplicity` hook already feeds back each edit's findings. Never an ad hoc "clean this up" brief.
- Advisory detectors (`claude-code review *`) run once per pipeline: the simplifier runs `simplify`+`diff`; the
  manager runs `pr`/`assumptions`/`goals` and pastes all output into the quality reviewer's brief; no later stage —
  `/code-review` included — re-runs them. The mechanical `make fmt && make lint` gate is separate and runs after every
  code change (`rules/standards/quality.md` → Verification Order).
- Leaves never auto-fallback. After ONE failed high-effort Sonnet attempt the manager re-dispatches with
  `model: 'opus'`; a cheap read-only sweep may retry on `haiku`. Tune `effort` per dispatch (low mechanical, medium
  default, high for one gnarly task) and drop effort before dropping a model; inherit-model managers never pin effort.

## Sequential plan

Per task: `code-worker` (‖ `copilot` for a feature or bug fix, same message) → spec review until `SPEC_COMPLIANT`
([prompts.md#spec-reviewer](references/prompts.md#spec-reviewer)) → `Skill(simplify)` → quality review until
`SHIP` ([prompts.md#code-quality-reviewer](references/prompts.md#code-quality-reviewer)) → run the task's
verification yourself. Order is always spec → simplify → quality: simplifying non-compliant code polishes the
wrong thing, and reviewing before simplifying reviews code about to change. Never overlap implementers.

Stop and ask the user when an assumption breaks (missing file, changed API), scope grows, or a decision falls
outside the plan.

## Test agent

Only when the change needs one. A feature or bug fix gets a test agent (`copilot`, or `code-worker` for a scoped suite)
dispatched in the SAME message as the implementer — dispatched afterwards, the concurrency is lost. Renames, moves,
deletions and refactors get none; the implementer keeps the build green. The main thread owns the live check
(`rules/standards/testing-guidelines.md`); the test agent repairs broken tests at once and writes new ones after it.
It ends its turn after taking the baseline; the manager's "landed" `SendMessage` (files + signatures) resumes it — no
polling. Roughly one per suite under change; it counts toward the cap. Not for exploration, docs or review-only work; a
target with no suite is reported, not given a new framework, and planned-but-unwritten tests are listed, never silent.
Reorg mode (`CLAUDE.md`) overrides this: no test agent, and the gate is `go build ./...` + `go vet`.

## After an interruption

After a session restart or a stopped leaf, run `git status` and read the leaf's files before anything else. Decide
keep or redo from the diff; never assume the work landed, and never relaunch a leaf the user stopped without asking.

## Shared tree

In `~/.claude` and any shared checkout, put in every brief: never `git stash` / `checkout` / `reset` /
`restore` (CLAUDE.md hard rule 4); gate on a scoped target (a repo-wide failure mid-fan-out is usually a sibling's
half-finished edit); confirm each write with `git diff --stat -- <file>` after writing and before reporting; stage
named files only. Prefer new-file ownership to `isolation: "worktree"` (setup cost; worktrees have vanished mid-run).

## Fan-in

Treat every report as a claim (verify on return). Before committing, read the leaf's diff and gate output, and check
every deleted block (callers, `git log -S`, plans, memory, docs). Inspect the integrated diff for edits outside each
agent's files; confirm a new guard test can fail (`testing-guidelines.md` → A test must guard); re-run the scoped checks
yourself (`-race` for concurrent code); rebuild before trusting a binary; simplify the integrated diff; report only
verified results.
