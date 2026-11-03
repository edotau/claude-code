---
name: subagent-workflows
description: Choose and run multi-agent work by task complexity — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, competition tournaments, multi-stage orchestration, and cheaper agent dispatches via `claude-code ask` — and the one handoff contract (brief in, report out, ownership, status). Use for "fan this out", "dispatch agents", "write a brief", "which agent", "fix failing tests", "test-and-fix loop", "regression tests", "does this test have teeth", "pytest", "contract test". Leaves run `problem-solving`, `evaluate-code` or `simplify`.
---

# Subagent Workflows

The dispatch procedure. The always-on core is `CLAUDE.md` guideline 3; the pipeline order is
`rules/workflows/master-workflow.md`. Use the smallest shape that fits:

| Shape | Do |
|---|---|
| Inline | A one-line edit; anything bigger is dispatched |
| Parallel leaves, disjoint files | `Agent` calls in one message ([dispatching-parallel-agents.md](references/dispatching-parallel-agents.md)) |
| Sequential plan | One implementer at a time, a gate per task ([Sequential plan](#sequential-plan)) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) |

Also: review a diff (`/code-review`) · [lane orchestrator](references/lane-orchestrator.md) ·
[orchestrate](references/orchestrate.md) · [tournament](references/competition-tournament.md) ·
[test-and-fix](references/test-and-fix.md) · [cost routing](references/cost-routing.md). Brief templates:
[prompts.md](references/prompts.md). Scope a leaf at the repo root: below it `.git`/`.venv` vanish.

## Leaf types

A leaf does the work itself on your pin (`code-worker`), or requests another agent or account you are signed in to
through `claude-code ask --agent <x> [--profile <p>]` (`coworker`, which always requests). A `coworker` is
**experimental**: an external account depends on its prompt and config being right and often cannot connect back, so
trust it only on the `ran on` line its `ask` prints (`dispatch-stop` blocks a stop with none, once)
and keep a `code-worker` fallback for work that must land.

Pass the task alone as `description`; the `dispatch-effort` hook prefixes the model id the leaf is planned to run
(`code-worker sonnet: <task>`, `coworker gpt-6.1-sol: <task>`) and appends memory-recall pointers to every brief. A
leaf's cost is its `model:` frontmatter: an `inherit` agent runs at the parent's model, so fan out as `code-worker`.

## Concurrency

At most 2 live leaves while other sessions share the checkout, else the account cap (`rules/harness/hooks.md` →
dispatch cap; test agents count). Independent work goes out in one message; run sequentially when one output feeds the
next, they share state, or a provider keeps erroring. Each leaf replays its context and tool schemas: a wide fan-out of
deep-context leaves spends a provider's tokens-per-minute quota (Databricks `429`), so batch those.

## Handoff contract

What passes manager → leaf → manager; [prompts.md](references/prompts.md) repeats the fields for cold agent CLIs.

**Brief (in)** — a fresh agent can act from it alone:

- `Objective:` one outcome. `Agent:` the agent requested via `ask` (omitted or `in-context` = does it itself; several
  names = relay `a → b` unless `Mode: compare`). `Profile:` a vault profile: the hook moves the dispatch to `coworker`;
  unknown names are denied. `Model:` is planned, not implemented.
- `Task:` the plan task id it lands (the hook claims it). `Owns:` the exact files, plus "Do NOT edit <others>".
- `Method:` the skill to follow. `Workflow:` a saved Workflow it may run, the only way a leaf starts one.
- `Context:` paths with lines, prior decisions, the plan path. `Constraints:` the CLAUDE.md rules that apply plus the
  [Shared tree](#shared-tree) line.
- `Acceptance:` an observable outcome. `Verify:` the scoped command and live check, never the full suite.

**Report (out)** — the outcome first: a review's verdict word alone, else one Status: `DONE` · `DONE_WITH_CONCERNS`
(resolve before review) · `NEEDS_CONTEXT` ([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never
resend unchanged). Then:

- `Ran on:` the quoted `ask: ran on …` line, or `self — <why>` in-context.
- Files changed, each verification command with its exit code, each `Acceptance:` item mapped to its evidence.
- `TASK:`/`ISSUE:` facts as plain sentences, anything found outside `Owns:`, tests planned but not written, optional
  [memory proposals](references/memory-updates.md).

**Ownership** — one writer per file; one package parallelizes when each leaf owns a new file and the manager makes the
integration edit. The manager owns decisions, integration, verification and commits. Leaves never dispatch subagents,
commit or run the suite; an `ask` request is a subprocess and allowed when the brief names it. Watch with
`claude-code agents tree` and `claude-code <agent> tail -f`.

## Return to owner

A gap goes back to the same agent with `SendMessage` (it resumes with its context): name the gap, the evidence wanted
and the stop condition. Wait on completion notifications; never poll, except a leaf's own long `ask`, which runs
`--background` and polls ([agent-verbs.md](references/agent-verbs.md)) because a foreground call idles at 180s.
Re-dispatch fresh only when the owner is gone. Never re-steer a running agent, except to pass on a requirement the
user changed; pick a cross-session recipient from `claude-code agents tree` by cwd and task, not by name. After a
session restart or a stopped leaf, read `git status` and its files first: keep or redo from the diff, never assume it
landed, and never relaunch a leaf the user stopped without asking.

## Rules

- Decide the owner before gathering evidence; finish inline only when the evidence is already in hand, and never repeat
  a running owner's searches.
- Simplify once per diff, before review: the manager runs `Skill(simplify)`; implementers never simplify their own.
- Advisory detectors (`claude-code review *`) run once per pipeline; `make fmt && make lint` runs after every change.
- Leaves never auto-fallback. After one failed high-effort Sonnet attempt, re-dispatch on `opus`; a read-only sweep may
  retry on `haiku`. Lower `effort` before lowering the model.

## Sequential plan

Per task: implementer → spec review until `SPEC_COMPLIANT` → `Skill(simplify)` → quality review until `SHIP` → run its
verification yourself ([prompts.md](references/prompts.md)). Simplifying non-compliant code polishes the wrong thing,
so the order never changes. Plans: `agents/Plan.md`; tasks tick from their `owns:` as commits land. Stop and ask when
an assumption breaks, scope grows, or a decision falls outside the plan.

## Test agent

A feature or bug fix gets a test agent (`copilot`, or `code-worker` for a scoped suite) in the SAME message as the
implementer; renames, moves, deletions and refactors get none. The main thread owns the live check; the test agent
repairs broken tests at once, ends its turn after the baseline and resumes on the manager's "landed" `SendMessage` to
write new ones. One per suite under change; it counts toward the cap; never for exploration, docs or review. A target
with no suite is reported, not given a framework; planned-but-unwritten tests are listed.

## Shared tree

Every brief for a shared checkout says: never `git stash` / `checkout` / `reset` / `restore`; gate on a scoped target
(a repo-wide red mid-fan-out is usually a sibling's half-finished edit); check each write with `git diff --stat`; stage
named files only. Prefer new-file ownership to `isolation: "worktree"`.

## Fan-in

Treat every report as a claim. Before committing, read the leaf's diff and gate output, check every deleted block
(callers, `git log -S`, plans, memory, docs) and edits outside its files, confirm a new test can fail, re-run the scoped
checks yourself (`-race` for concurrent code), and rebuild before trusting a binary. Report only verified results.
