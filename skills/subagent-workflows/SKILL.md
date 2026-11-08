---
name: subagent-workflows
description: Run multi-agent work end to end, from picking the shape (fan-out on disjoint files, sequential plan, lane orchestrator, tournament, saved Workflow), write briefs, dispatch, fan in, commit. Use for "fan this out", "dispatch agents", "write a brief", "which agent", "run the plan", "compare agents", "fix failing tests", "regression tests", "does this test have teeth", and follow-ups like "re-run the failed leaf", "only the X task again", "continue the plan", "redo with codex". Not one diff review (`/code-review`) or planning (`Plan`).
---

# Subagent Workflows

## Execution mode: subagent

Leaves run through the Agent tool or `claude-code agents ask`; they are terminal and never fan out, so the manager
(the main session) is the one orchestration layer. Agent files say who a leaf is, the brief's `Method:` skill how,
this skill when and in what order, [patterns.md](references/patterns.md) which shape: weak output means fix the
skill, a wrong role the agent file, a wrong order this skill. Writing an agent file: `rules/workflows/agents.md`.

## Who holds the plan

| Shape | The plan lives in | Use when |
| --- | --- | --- |
| Subagent / skill | Claude, turn by turn | the next step depends on what the last one found |
| Agent team (experimental) | a lead over peers | peers must talk to each other; not used here |
| Workflow | the script | the steps are known up front and must run the same every time |

Source: [when to use a workflow](https://code.claude.com/docs/en/workflows#when-to-use-a-workflow). When the owner asks
for a workflow and no specific one fits, run `code-engineer` (`~/.claude/workflows/code-engineer.js`).

## Leaf types

A leaf's `subagent_type` is the agent doing the work: `claude` works in-context; `codex`, `copilot`, `gemini` and
`opencode` request themselves via `claude-code agents ask --agent <x>` (**experimental**). Trust a request only on the
`ran on` line its `ask` prints (`dispatch-stop` blocks a stop with none, once); keep a `claude` fallback.

| Leaf | subagent_type | Role | Method skill | Output |
| --- | --- | --- | --- | --- |
| Claude | `claude` | Default: in-context Sonnet work, reasoning, integration | per brief | Status report |
| Codex · Gemini | `codex` · `gemini` | Diff or plan review · read-heavy sweeps | `evaluate-code` | Verdict · file:line findings |
| Copilot · opencode | `copilot` · `opencode` | Mechanical edits, test repair · edits on discovered models | per brief | Status report |
| Databricks | any type + `Profile: <name>` | Force the agent onto a Databricks profile; the type is unchanged | per brief | Status report |
| Explore | `Explore` | Read-only scan before any non-trivial work | none | Codebase Context Brief |
| Plan | `Plan` | Design the checklist the leaves execute | `plans/` template | Plan file |
| Test agent | `copilot` (TEST mode) | Repair and write tests beside the implementer | `testing-guidelines.md` | Test report |

The `dispatch-effort` hook prefixes the `description` with the planned model (`gpt-6.1-sol: <task>`; in auto
mode it denies once naming the text to resend); an `inherit` agent runs at the parent's model: fan out as `claude`.

## Workflow

### Phase 0: intake and triage

The Phase 0 tier in `rules/workflows/master-workflow.md` picks the shape; trivial work (a one-line edit) runs inline.
Then check context: a fresh run, a follow-up (re-run one leaf with its prior report and `git diff`), or a continued
plan (resume at the first unticked line of its `~/.claude/plans/` checklist).

| Shape | Do |
|---|---|
| Single leaf | One `Agent` call with a brief ([prompts.md](references/prompts.md)) |
| Parallel fan-out | `Agent` calls in one message, disjoint files ([Ownership](#handoff-contract)) |
| Same question, many angles | `claude-code agents dispatch explore\|fleet\|compare` ([cost-routing.md](references/cost-routing.md)) |
| Sequential plan · tournament | [patterns.md](references/patterns.md) (every shape, its pattern and what enforces it) |
| Many leaf-sized tasks | [Lane orchestrator](references/lane-orchestrator.md) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) · composite templates: [composites.md](references/composites.md) |
| Fleet scan | `claude-code agents dispatch fleet "<task>"`; check `git diff --stat` after |

### Phase 1: prepare

Give each leaf named files it owns and never share one between parallel leaves, so no write is lost. Split or merge
leaves by expertise, independence, context load and reuse; prefer a few focused ones: each extra leaf costs a brief
and a fan-in read.

#### Handoff contract

**Brief (in)** — a fresh agent can act from it alone:

- `Objective:` one outcome. `Agent:` the agent requested via `ask` (omitted or `in-context` = does it itself; several
  names = relay `a → b` unless `Mode: compare`). `Profile:` a vault profile (the dispatch keeps its type; unknown
  names are denied). `Model:` is planned, not implemented. `Task:` the plan task id it lands (the hook claims it).
- `Owns:` the exact files, plus "Do NOT edit <others>". `Method:` the skill to follow. `Workflow:` a saved Workflow it
  may run, the only way a leaf starts one. `Context:` paths with lines, decisions, plan path. `Constraints:` CLAUDE.md
  rules plus the [Shared tree](#shared-tree) line. `Acceptance:` an observable outcome. `Verify:` the scoped command.

**Report (out)** — a review's verdict word alone, else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve before
review) · `NEEDS_CONTEXT` ([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend unchanged).
Then `Ran on:` (the `ask: ran on …` line, or `self — <why>`), files, commands with exit codes, `Acceptance:` evidence.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface, and the manager makes the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit or run the suite (an `ask` is allowed).

### Phase 2: dispatch

Send independent leaves in one message with `run_in_background`; run sequentially when one output feeds the next.

#### Concurrency

The cap is 24 per account via the dispatch hook, test agents included (`rules/harness/hooks.md` → dispatch cap).
Parallel leaves never share a file; batch deep-context leaves (Databricks `429`) and fleet scans.

#### Test agent

A feature or bug fix gets a test agent (`copilot`, or `claude` for a scoped suite) in the SAME message as the
implementer (refactors get none). The main thread owns the live check; the test agent repairs broken tests at once, ends
its turn after the baseline, and resumes on the manager's "landed" `SendMessage`, testing the seam each time a piece
lands, not only at the end. One per suite, never for review. Loop: `rules/standards/testing-guidelines.md`. A leaf at
its cap (3 cycles per test, 5 per task) reports and stops.

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one `copilot` leaf |
| Failures across packages | one `copilot` leaf per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | `claude` ‖ `copilot` in one message; copilot repairs now, writes new tests after "landed" |
| A fixed bug | after your live before/after, one leaf writes the regression test + `test mutate` proof |
| Suite must go green, bounded | `Workflow green-keeper-loop` — only when the user asks |

### Phase 3: integrate

#### Fan-in

Treat every report as a claim: read the diff and gate output, check deleted blocks (callers, `git log -S`) and edits
outside the leaf's files, confirm a new test can fail, re-run scoped checks yourself, rebuild before trusting a binary.

- When parallel leaves built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔ brief
  writer), read producer and consumer together; `go build ./...` misses a broken consumer.
- Disagreeing reviewers: keep both findings with their sources. Simplify once over the merged diff, then review.

### Phase 4: finish

Run the verification order in `rules/standards/quality.md`, then commit named files only (`git commit -- <paths>`, so a
staged deletion from elsewhere is never swept in) and report to the owner.

#### Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or `sleep`:
a long `ask` runs `--background`, then waits with `tail -f <id>` as a background Bash call
([agent-verbs.md](references/agent-verbs.md) step 3). Re-dispatch only when the owner is gone; after a restart read
`git status`; never relaunch a leaf the user stopped. Reports go to `$(claude-code memory path)/reports/`.

## Shared tree

Every brief says: never `git stash` / `checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); check writes with `git diff --stat`; stage named files only.

## Rules

- Decide the owner before gathering evidence, never redoing its searches. Simplify once per diff, before review, run by
  the manager; detectors (`claude-code review *`) once per pipeline.
- Leaves never auto-fallback: after one failed high-effort Sonnet attempt re-dispatch on `opus` (a read-only sweep may
  retry on `haiku`); lower `effort` before the model.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Retry once with its failure output and a changed brief; a second failure: land the rest, name the gap |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Timeout | Keep partial results, cancel the rest |
| Reviewers disagree | Keep both findings with their sources |
| A leaf touched a file it didn't own | Revert that hunk, re-dispatch with the `Owns:` line sharpened |
| External agent unavailable (credential or route missing, e.g. a Databricks profile without its M2M secret) | Report BLOCKED with the fix command; never silently do it in-context |
| Quota pressure | `dispatch-effort` moves work to `copilot` ([cost-routing.md](references/cost-routing.md)) |

## Test scenarios

- **Normal**: two `claude` leaves on disjoint packages plus a `copilot` test agent go out in one message. Fan-in
  reads each diff and the seam between them, the test agent runs on "landed", simplify runs once, then one commit names
  the files.
- **Error**: a leaf fails its `Verify:` twice (second time with the failure output and a changed brief). The other
  leaves land, the report names the missing task as a gap, and nothing is silently redone in-context.

## Authoring rules

- Name `Agent:`, `Mode:`, `Owns:` and `Verify:` in every brief; the hooks and fan-in read them.
- One task per leaf; state inter-task dependencies so the manager orders the dispatch.
- Paste every fact the leaf needs: the CLI starts cold and cannot see this session.
- Specify realistic error handling: what to report on a missing credential, a red gate or a file it doesn't own.
- Choose models in the `ask` call (`--model`), not a brief field; `Model:` is planned, not implemented.
