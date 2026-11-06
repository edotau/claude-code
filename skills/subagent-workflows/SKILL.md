---
name: subagent-workflows
description: Choose and run multi-agent work by task complexity — parallel fan-out on disjoint files, sequential plans with spec → simplify → quality gates, lane orchestrators, tournaments, saved Workflows, cheaper dispatches via `claude-code ask` — and the one handoff contract (brief in, report out). Use for "fan this out", "dispatch agents", "write a brief", "which agent", "fix failing tests", "regression tests", "does this test have teeth". Not for reviewing one diff (`/code-review`) or planning (`Plan`).
---

# Subagent Workflows

Agent files say who a leaf is, the brief's `Method:` skill how, this skill when and in what order (weak output: fix
the skill; wrong role: the agent file). The Phase 0 tier in `rules/workflows/master-workflow.md` picks the shape;
anything bigger than a one-line edit is dispatched, templates in [prompts.md](references/prompts.md):

| Shape | Do |
|---|---|
| Same question, many angles | `claude-code dispatch explore\|fleet\|compare` ([cost-routing.md](references/cost-routing.md)) |
| Parallel leaves, disjoint files | `Agent` calls in one message ([Ownership](#handoff-contract)); many leaf-sized tasks: [lane orchestrator](references/lane-orchestrator.md) |
| Dependent tasks · measurable goal with diverging approaches | [Sequential plan](#sequential-plan) · [Tournament](#tournament) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) |

## Leaf types

A leaf works itself on your pin (`code-worker`) or requests another agent via `claude-code ask --agent <x>`
(`coworker`, **experimental**); bare `code-worker`/`coworker` take the brief's `Agent:`. Trust a request only on the
`ran on` line its `ask` prints (`dispatch-stop` blocks a stop with none, once); keep a `code-worker` fallback.

| Type | Use when |
| --- | --- |
| `code-worker:claude` | Default leaf: in-context Sonnet work, reasoning, integration |
| `code-worker:codex` · `code-worker:gemini` | Review or second opinion on a diff or plan · read-heavy sweeps, cite file:line |
| `coworker:copilot` · `coworker:opencode` | Mechanical edits and test repair (Copilot); edits on discovered models (opencode) |
| `coworker:databricks` | Any agent forced onto a Databricks profile (`agent=` in the brief, default claude) |

The `dispatch-effort` hook prefixes the `description` with the planned model (`coworker gpt-6.1-sol: <task>`); an
`inherit` agent runs at the parent's model: fan out as `code-worker`.

## Concurrency

The cap is 24 per account, test agents included (`rules/harness/hooks.md` → dispatch cap). Parallel leaves never share
a file; run sequentially when one output feeds the next; batch deep-context leaves (Databricks `429`).

## Handoff contract

**Brief (in)** — a fresh agent can act from it alone:

- `Objective:` one outcome. `Agent:` the agent requested via `ask` (omitted or `in-context` = does it itself; several
  names = relay `a → b` unless `Mode: compare`). `Profile:` a vault profile (moves the dispatch to `coworker`; unknown
  names are denied). `Model:` is planned, not implemented. `Task:` the plan task id it lands (the hook claims it).
- `Owns:` the exact files, plus "Do NOT edit <others>". `Method:` the skill to follow. `Workflow:` a saved Workflow it
  may run, the only way a leaf starts one. `Context:` paths with lines, decisions, plan path. `Constraints:` CLAUDE.md
  rules plus the [Shared tree](#shared-tree) line. `Acceptance:` an observable outcome. `Verify:` the scoped command.

**Report (out)** — a review's verdict word alone, else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve before
review) · `NEEDS_CONTEXT` ([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend unchanged).
Then `Ran on:` (quoted `ask: ran on …` line, or `self — <why>`), files, commands with exit codes, `Acceptance:` evidence.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface, and the manager makes the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit or run the suite (an `ask` is allowed). Split or
merge leaves by expertise, independence, context load and reuse; prefer a few focused ones.

## Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or `sleep`:
a leaf's long `ask` runs `--background`, then waits with `tail -f <id>` as a background Bash call
([agent-verbs.md](references/agent-verbs.md) step 3). Re-dispatch only when the owner is gone; after a restart read
`git status`; never relaunch a leaf the user stopped.

## Rules

- Decide the owner before gathering evidence, never repeating its searches. Simplify once per diff, before review, run by
  the manager; detectors (`claude-code review *`) once per pipeline.
- Leaves never auto-fallback: after one failed high-effort Sonnet attempt re-dispatch on `opus` (a read-only sweep may
  retry on `haiku`); lower `effort` before the model.

## Sequential plan

Per task: implementer → spec review until `SPEC_COMPLIANT` → `Skill(simplify)` → quality review until `SHIP` → run its
verification yourself ([prompts.md](references/prompts.md)). Stop and ask when an assumption breaks, scope grows, or a
decision falls outside the plan.

## Test agent

A feature or bug fix gets a test agent (`copilot`, or `code-worker` for a scoped suite) in the SAME message as the
implementer (refactors get none). The main thread owns the live check; the test agent repairs broken tests at once, ends
its turn after the baseline, and resumes on the manager's "landed" `SendMessage`, testing the seam each time a piece
lands, not only at the end. One per suite, never for review. Loop: `rules/standards/testing-guidelines.md`. A leaf at
its cap (3 cycles per test, 5 per task) reports and stops.

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one `copilot` leaf |
| Failures across packages | one `copilot` leaf per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | `code-worker` ‖ `copilot` in one message; copilot repairs now, writes new tests after "landed" |
| A fixed bug | after your live before/after, one leaf writes the regression test + `test mutate` proof |
| Suite must go green, bounded | `Workflow green-keeper-loop` — only when the user asks |

## Shared tree

Every brief says: never `git stash` / `checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); check writes with `git diff --stat`; stage named files only.

## Tournament

For a measurable goal (latency, size, pass rate) or truly diverging approaches worth N× the cost, within the cap:

- Baseline the eval; send one brief verbatim to N `code-worker`s (`isolation: "worktree"`, branch `compete/<slug>/<n>`,
  one named constraint varied). Answers only: `dispatch compare --models a,b`.
- Re-measure yourself, discard red, rank on the diff: Metric (eval vs baseline) · Judge (correctness → simplicity →
  quality) · Hybrid (metric, then judge leaves within ~10%). `claude-code worktree merge` the winner, re-run the gates,
  `git tag compete/<slug>/<n>` the losers. All red → merge nothing; none beats baseline (a tie is not a win) → keep it.

## Fan-in

Treat every report as a claim: read the diff and gate output, check deleted blocks (callers, `git log -S`) and edits
outside the leaf's files, confirm a new test can fail, re-run scoped checks yourself, rebuild before trusting a binary.

- Retry a red leaf once: one re-dispatch carrying the failure output and a changed brief. A second failure: land the
  rest and name the gap, never drop it silently; a timeout keeps partial results. Disagreeing reviewers: keep both
  findings with their sources.
- When parallel leaves built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔ brief
  writer), read producer and consumer together; `go build ./...` misses a broken consumer.
