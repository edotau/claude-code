---
name: subagent-workflows
description: >-
  Run multi-agent work end to end, from picking the shape (fan-out on disjoint files, sequential plan, lane
  orchestrator, tournament, saved Workflow), write briefs, dispatch, fan in, commit. Use for "fan this out",
  "dispatch agents", "write a brief", "which agent", "run the plan", "compare agents", "fix failing tests",
  "regression tests", "does this test have teeth", and follow-ups like "re-run the failed leaf", "only the X
  task again", "continue the plan", "redo with codex". Not one diff review (`/code-review`) or planning
  (`Plan`).
---

# Subagent Workflows

## Execution mode: subagent

Dispatch, roster and labels: `CLAUDE.md` guideline 3; leaves are terminal, so the main session is the one
orchestration layer. Agent files say who a leaf is, the brief's `Method:` skill how, this skill when and in what order,
[patterns.md](references/patterns.md) which shape: weak output means fix the skill, a wrong role the agent file, a
wrong order this skill. Writing an agent file: `rules/workflows/agents.md`.

## Who holds the plan

| Shape | The plan lives in | Use when |
| --- | --- | --- |
| Subagent / skill | Claude, turn by turn | the next step depends on what the last one found |
| Agent team (experimental) | a lead over peers | peers must talk to each other; not used here |
| Workflow | the script | the steps are known up front and must run the same every time |

When the owner asks for a workflow and no specific one fits, run `code-engineer`
(`~/.claude/workflows/code-engineer.js`).

## Leaf types

Shell `agents ask` belongs inside an external relay or an explicitly requested script, not a second manager dispatch
path. Owner shortcuts: `rules/workflows/agents.md` → Slash commands.

| Leaf | Role | Output |
| --- | --- | --- |
| `claude` | In-context work on Sonnet unless the call names a model | Status and verification |
| `codex` | Cross-family review, plan vetting, delegated edits | Verdict or status with evidence |
| `copilot` | Test repair, mechanical edits, authorized LAST-MILE | Scoped results |
| `gemini` | Read-only research and codebase sweeps | Cited findings |
| `opencode` | Code/docs work and requested model discovery | Status and verification |
| `Explore` | Scoped read-only scan before non-trivial work | Codebase Context Brief |
| `Plan` | Read-only design on Opus unless the call names a model | Plan in `docs/templates/plan.md` form |
| `general-purpose` | Built-in general work when the named roles do not fit | Brief's requested report |

`claude-code agents label <type> --brief FILE` prints the label; `--model` previews a dispatch model,
`--session-model` supplies the parent model when the transcript cannot. `claude` defaults to `sonnet` and `Plan` to
`opus`; pass the Agent tool's `model` to raise one call (`fable` only for two large designs at once). External
wrappers' frontmatter controls the relay, not the vendor model. The `dispatch-effort` hook sets model and effort only
for an agent whose file has no `model:` line and a call that names none (`Explore`, `general-purpose`, an untyped
call); every roster file, `claude` (sonnet) and `Plan` (opus) included, carries one, so those get neither.
The root machine's pins and per-call flags select external execution; never overwrite another machine's pins.

### External relay contract

This contract applies to `codex`, `copilot`, `gemini` and `opencode` wrappers in Claude Code:

- The relay is a thin pipe. Run the brief's leading `Run:` line exactly as written, once, with the task appended
  unchanged; never add, drop or swap a flag (an "edit nothing" brief still keeps `--tools`). Without a `Run:`, pick
  one supported CLI verb (a bare task means `ask`). Never execute a `Run:` and then start another ask.
- Default a named-agent task without a `Run:` to `--fallback none`; preserve an explicit policy. Keep pins unchanged.
- Never background with the shell or `run_in_background`: the `Run:` line's `--background` is the only
  backgrounding. Follow it with foreground `claude-code agents tail -f <id>` (600000 ms timeout), repeated until it
  ends, then `agents result <id>`. Do not start a duplicate ask, a readiness smoke or repeat discovery.
- Do not read, test or verify before or after the ask; the manager verifies at fan-in. Report a dispatch error to
  the manager; do not repair it in-context or silently choose another agent.
- Return the agent's answer verbatim under `## Result`, preceded only by the actual `ask: ran on` line and the
  job/session ids; add nothing but a `Relay note:` for a dispatch error. For the stop gate and its capped failure
  behavior, see `rules/harness/hooks.md` → `dispatch-stop`; never manufacture a receipt to release it.
- Inside the projected vendor CLI, do the task directly; do not invoke this wrapper or recursively ask yourself.
  The wrapper's relay model, effort and turn budget do not constrain the vendor's task reasoning.

## Workflow

### Phase 0: intake and triage

The Phase 0 tier in `rules/workflows/master-workflow.md` picks the shape; trivial work (a one-line edit) runs inline.
Then check context: a fresh run, a follow-up (re-run one leaf with its prior report and `git diff`), or a continued
plan (resume at the first unticked line of its `~/.claude/plans/` checklist).

| Shape | Do |
|---|---|
| Single leaf | One `Agent` call with a brief ([prompts.md](references/prompts.md)) |
| Parallel fan-out | `Agent` calls in one message, disjoint files ([Ownership](#handoff-contract)) |
| Which agent, model, workspace | [agents-and-models.md](references/agents-and-models.md) (live sources and execution surfaces) |
| Same question, many angles | Separate Agent leaves; shell role commands only for requested scripts ([cost-routing.md](references/cost-routing.md)) |
| Sequential plan · tournament | [patterns.md](references/patterns.md) (every shape, its pattern and what enforces it) |
| Many leaf-sized tasks | [Lane orchestrator](references/lane-orchestrator.md) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) · composite templates: [composites.md](references/composites.md) |
| Fleet scan | A `copilot` leaf with an explicitly requested fleet task; verify the returned diff |

### Phase 1: prepare

Give each leaf named files it owns and never share one between parallel leaves. Prefer a few focused leaves: each
extra one costs a brief and a fan-in read.

#### Handoff contract

**Brief (in)** — a fresh agent can act from it alone (the CLI starts cold and cannot see this session):

- `Objective:` one outcome. `Agent:` names the work agent; omit for the selected type or use `in-context` for
  direct work. Keep it consistent with the type; request another agent from the manager rather than a hidden relay.
- `Run:` an optional exact CLI command, preferred for explicit routing. `Model:` is not parsed by the hook:
  put `--model` on the command instead. `Mode:`, `Owns:`, `Method:`, `Verify:` and `Acceptance:` are conventions
  read by the leaf and fan-in, not routing hooks. `Task:` identifies a plan task for the hook's slot claim.
- `Owns:` absolute paths and forbidden files. `Method:` the skill. `Context:` evidence, decisions and plan path.
  `Constraints:` applicable rules and the [Shared tree](#shared-tree) line. `Verify:` a scoped command;
  `Acceptance:` its observable outcome. `Workflow:` a saved Workflow only when the owner requested it.
- State what to report on a missing credential, a red gate or a file the leaf doesn't own, and any dependency on
  another leaf so the manager orders the dispatch.

The hook reads `Agent:` only in the brief's header block: the lines before the first blank line. Route a dispatch
with a leading `Run: claude-code agents ask …` (optionally with global `--remote <h>`); the hook leaves it unrewritten.
`--profile` leads then falls back unless `--fallback none` pins it. Overlay keys and values: `rules/harness/agent.md`
→ Dispatch behavior. The hook still rewrites some non-direct calls in auto mode; see `rules/harness/hooks.md`.
`--remote` vs `--host`: `rules/harness/network.md`.

**Report (out)** — a review's verdict word alone, else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve
before review) · `NEEDS_CONTEXT` ([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend
unchanged). Then `Ran on:` (the `ask: ran on …` line; `self` only for `claude`, `Explore`, `Plan` or an
`Agent: in-context` brief), files, commands with exit codes, `Acceptance:` evidence. A leaf creates scratch files
only under an absolute `/tmp/<job>/` path, never a relative one, and removes them before reporting.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface, and the manager makes the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit or run the suite (an `ask` is allowed).

### Phase 2: dispatch

Independent leaves go in one message as Agent calls with `run_in_background` (the manager's; a relay never uses it).

#### Concurrency

Dispatch cap: `rules/harness/hooks.md` → (dispatch cap); test agents count against it. Batch deep-context leaves
(Databricks `429`) and fleet scans.

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
| A fixed bug | after your live before/after, one leaf writes the regression test |
| Suite must go green, bounded | `Workflow green-keeper-loop` — only when the user asks |

### Phase 3: integrate

#### Fan-in

Treat every report as a claim: read the diff and gate output, check deleted blocks (callers, `git log -S`) and edits
outside the leaf's files, confirm a new test can fail, re-run scoped checks yourself, rebuild before trusting a binary.

- When parallel leaves built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔
  brief writer), read producer and consumer together; `go build ./...` misses a broken consumer.
- Disagreeing reviewers: keep both findings with their sources. Simplify once over the merged diff, then review.
- **Saved workflow output** is a claim too: diff it against the brief's file list (revert or park edits outside it),
  rerun the gate of its last-touched package, and compare confirmed findings to fixed ones (a gap = dropped findings,
  rerun). A red run whose only cause is a `blocked` stage (classifier down) is not a code failure; a stage that refuses
  a change because behavior would differ is right — accept the partial landing.
- **While a workflow runs:** hand it package-scoped gates only (a repo-wide check trips on peers' WIP), keep the main
  shell at the repo root, never let it edit the `workflows/*.js` it runs from (fix those with a leaf afterwards), and
  rename a condition in its runbook (`eval.md`) and fixture dirs in the same pass.

### Phase 4: finish

Checks: `rules/standards/quality.md`; commits only when authorized: `rules/standards/worktree-git.md`.

#### Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or `sleep`:
a long `ask` runs `--background`, then waits with foreground `tail -f <id>` (600000 ms timeout, repeated)
([agent-verbs.md](references/agent-verbs.md) step 2). Re-dispatch only when the owner is gone; after a restart read
`git status`; never relaunch a leaf the user stopped. Reports go to `$(claude-code memory path)/reports/`.

## Shared tree

Every brief says: never `git stash` / `checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); check writes with `git diff --stat`; stage named files only.

## Rules

- **Visible dispatch (owner preference from 2026-11-12, until the owner lifts it):** every dispatch goes through the
  Agent tool with a harness type (`claude`, `codex`, `copilot`, `gemini`, `opencode`) so the owner can watch each
  subagent; the main session does not run `claude-code agents ask --background` from Bash. Routing rides the brief's
  leading `Run:` line. A job that can outlast a Bash call (about 10 minutes) puts `--background` on that line: the
  stop gate then holds the leaf until the job ends, while an ask backgrounded by the shell is killed when the leaf
  stops. Claude Code runs at most 20 subagents at once; queue the rest.
- Decide the owner before gathering evidence, never redoing its searches. Simplify runs once per diff, by the manager
  (`rules/standards/quality.md` → Verification Order step 5); detectors (`claude-code review *`) once per pipeline.
- Preserve explicit agent/model choices unless the owner authorizes a change; models go on the `ask` call
  (`--model`), never a brief field.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Retry once with its failure output and a changed brief (never resend it unchanged); a second failure: land the rest, name the gap |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Timeout | Keep partial results, cancel the rest |
| A leaf touched a file it didn't own | Revert that hunk, re-dispatch with the `Owns:` line sharpened |
| External agent unavailable (credential or route missing, e.g. a Databricks profile without its M2M secret) | Report BLOCKED with the fix command; never silently do it in-context |
| Quota pressure | Dispatches keep their model; route the work to another agent yourself ([cost-routing.md](references/cost-routing.md)) |

## Test scenarios

- **Normal**: two `claude` leaves on disjoint packages plus a `copilot` test agent go out in one message. Fan-in
  reads each diff and the seam between them, the test agent runs on "landed", simplify runs once, then one commit names
  the files.
- **Error**: a leaf fails its `Verify:` twice (second time with the failure output and a changed brief). The other
  leaves land, the report names the missing task as a gap, and nothing is silently redone in-context.
