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
orchestration layer. Agent file = who, brief's `Method:` skill = how, this skill = when and in what order,
[patterns.md](references/patterns.md) = which shape. Agent files: `rules/workflows/agents.md`.

## Who holds the plan

| Shape | The plan lives in | Use when |
| --- | --- | --- |
| Subagent / skill | Claude, turn by turn | the next step depends on what the last one found |
| Agent team (experimental) | a lead over peers | peers must talk to each other; not used here |
| Workflow | the script | the steps are known up front and must run the same every time |

Workflow requested, none fits: run `code-engineer` (`~/.claude/workflows/code-engineer.js`).

## Leaf types

Shell `agents ask` belongs only inside an external relay or a requested script. Owner shortcuts:
`rules/workflows/agents.md` → Slash commands.

| Leaf | Role | Output |
| --- | --- | --- |
| `claude` | In-context work on Sonnet unless the call names a model | Status and verification |
| `codex` | Cross-family review, plan vetting, delegated edits | Verdict or status with evidence |
| `copilot` | Test repair, mechanical edits, authorized LAST-MILE | Scoped results |
| `gemini` | Read-only research and codebase sweeps | Cited findings |
| `opencode` | Code/docs work and requested model discovery | Status and verification |
| `Explore` | Scoped read-only scan before non-trivial work | Codebase Context Brief |
| `Plan` | Read-only design on the session's model unless the call names one | Plan in `docs/templates/plan.md` form |
| `general-purpose` | Built-in work when no named role fits | Brief's requested report |

`claude-code agents label <type> --task FILE` prints the label (`--model` previews a model, `--session-model` supplies
the parent's). Pass the Agent tool's `model` to raise one call (`fable` only for two large designs at once). Wrapper
frontmatter controls the relay, not the vendor model. `dispatch-effort` sets model and effort only for an agent with no
`model:` line and a call naming none (`Explore`, `general-purpose`, untyped). The root machine's pins and per-call flags
select external execution; never overwrite another machine's pins.

### External relay contract

For `codex`, `copilot`, `gemini` and `opencode` wrappers:

- A thin pipe: run the brief's leading `Run:` line exactly as written, once, task appended unchanged; never add, drop
  or swap a flag (an "edit nothing" brief keeps `--tools`) or start another ask. Without a `Run:`, pick one supported
  verb (a bare task means `ask`), default a named-agent task to `--fallback none`, keep explicit policy and pins.
- Background only via the `Run:` line's `--background` (never the shell or `run_in_background`): follow with foreground
  `claude-code agents result -f <id>` (600000 ms, repeated until it ends). No duplicate ask,
  readiness smoke or repeat discovery.
- Do not read, test or verify around the ask; the manager verifies at fan-in. Report a dispatch error; never repair it
  in-context or silently pick another agent.
- Return the agent's verdict (Report (out)), not its whole answer, under `## Result`, preceded only by the actual
  `ask: ran on` line and job/session ids, plus a `Relay note:` for a dispatch error. Stop gate:
  `rules/harness/hooks.md` → `dispatch-stop`; never manufacture a receipt.
- Inside the vendor CLI, do the task directly; never invoke the wrapper or ask yourself.

## Workflow

### Phase 0: intake and triage

The tier in `rules/workflows/master-workflow.md` → Phase 0 picks the shape; a one-line edit runs inline. Then check
context: follow-up (re-run one leaf with its prior report and `git diff`) or continued plan (resume at the first
unticked line of its `~/.claude/plans/` checklist).

| Shape | Do |
|---|---|
| Single leaf | One `Agent` call with a brief ([prompts.md](references/prompts.md)) |
| Parallel fan-out | `Agent` calls in one message, disjoint files ([Ownership](#handoff-contract)) |
| Which agent, model, workspace | [agents-and-models.md](references/agents-and-models.md) |
| Same question, many angles | Separate Agent leaves; shell role commands only for requested scripts ([cost-routing.md](references/cost-routing.md)) |
| Sequential plan · tournament | [patterns.md](references/patterns.md) |
| A plan's waves, observed | `claude-code agents dispatch --waves <plan>` (a flag, not a verb; never commits): exit 0 passed, 1 failed wave, 2 invalid plan, 3 file outside a package's owned set or unattributed, 4 interrupted; a failed wave reruns whole, skipping packages whose verification passed. From a script: `--background` prints an id, then `agents wait <id> --timeout 10m` (exit 4 = still running, wait again) |
| Many leaf-sized tasks | [Lane orchestrator](references/lane-orchestrator.md) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) · templates: [composites.md](references/composites.md) |
| Fleet scan | A `copilot` leaf with an explicitly requested fleet task; verify the returned diff |

### Phase 1: prepare

Give each leaf named files it owns, never shared between parallel leaves. Fan out as many leaves as the task allows
(`CLAUDE.md` guideline 3): split until files would overlap or one leaf must wait on another's result.

#### Handoff contract

**Brief (in)** — a fresh agent can act from it alone (the CLI starts cold and cannot see this session):

- `Objective:` one outcome. `Agent:` the work agent; omit for the selected type, `in-context` for direct work; keep
  it consistent with the type (no hidden relay).
- `Run:` optional exact CLI command, preferred for explicit routing. `Model:` is not parsed by the hook: put `--model`
  on the command. `Mode:`, `Owns:`, `Method:`, `Verify:`, `Acceptance:` are conventions, not routing hooks. `Task:`
  identifies a plan task for the hook's slot claim.
- `Owns:` absolute paths and forbidden files. `Method:` the skill. `Context:` evidence, decisions, plan path.
  `Constraints:` applicable rules and the [Shared tree](#shared-tree) line. `Verify:` a scoped command;
  `Acceptance:` its observable outcome. `Workflow:` a saved Workflow, only when the owner requested it.
- State what to report on a missing credential, a red gate or a file the leaf doesn't own, and any dependency on
  another leaf.

The hook reads `Agent:` only before the first blank line. A leading `Run: claude-code agents ask …` (optionally with
global `--remote <h>`) is left unrewritten; `--profile` leads then falls back unless `--fallback none` pins it. Overlay
keys: `rules/harness/agent.md` → Dispatch behavior; auto-mode rewrites: `rules/harness/hooks.md`; `--remote` vs
`--host`: `rules/harness/network.md`.

**Report (out)** — the final answer is a verdict of at most 8 lines (about 2,500 characters): a review's verdict
word, else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve before review) · `NEEDS_CONTEXT`
([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend unchanged); then `Ran on:` (the
`ask: ran on …` line; `self` only for `claude`, `Explore`, `Plan` or an `Agent: in-context` brief), outcome per item,
test or gate results, what needs a decision, and the report path. The full report (root cause, before/after output,
numstat, teeth proof, commands) goes to `<store>/reports/YYYY-MM-DD-<name>.md` (`claude-code memory path`), wrapped
at 118 columns. Scratch files only under an absolute `/tmp/<job>/` path, removed before reporting.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface, the manager making the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit or run the suite (an `ask` is allowed).

### Phase 2: dispatch

Independent leaves go in one message as Agent calls with `run_in_background` (the manager's; a relay never uses it).

#### Concurrency

Cap: `rules/harness/hooks.md` → (dispatch cap); test agents count. Batch deep-context leaves (Databricks `429`) and
fleet scans.

#### Test agent

A feature or bug fix gets a test agent (`copilot`, or `claude` for a scoped suite) in the SAME message as the
implementer (refactors get none). The main thread owns the live check; the test agent repairs broken tests at once, ends
its turn after the baseline, and resumes on the manager's "landed" `SendMessage`, testing the seam as each piece lands.
One per suite, never for review. Loop: `rules/standards/testing-guidelines.md`. A leaf at its cap (3 cycles per test,
5 per task) reports and stops.

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one `copilot` leaf |
| Failures across packages | one `copilot` leaf per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | `claude` ‖ `copilot` in one message; copilot repairs now, writes new tests after "landed" |
| A fixed bug | after your live before/after, one leaf writes the regression test |
| Suite must go green, bounded | `Workflow code-looper` — only when the user asks |

### Phase 3: integrate

#### Fan-in

Treat every report as a claim: read the diff and gate output, check deleted blocks (callers, `git log -S`) and edits
outside the leaf's files, confirm a new test can fail, re-run scoped checks yourself, rebuild before trusting a binary.

- Parallel leaves that built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔
  brief writer): read both together; `go build ./...` misses a broken consumer.
- Disagreeing reviewers: keep both findings with sources. Simplify once over the merged diff, then review.
- **Saved workflow output** is a claim too, and a workflow's run has its own rules:
  [saved-workflows.md](references/saved-workflows.md)
  → Fan-in and run rules.

### Phase 4: finish

Checks: `rules/standards/quality.md`; commits only when authorized: `rules/standards/worktree-git.md`.

#### Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or
`sleep`: a long `ask` runs `--background`, then foreground `result -f <id>` (600000 ms, repeated;
[agent-verbs.md](references/agent-verbs.md) step 2). Re-dispatch only when the owner is gone; after a restart read
`git status`; never relaunch a leaf the user stopped. Reports go to `$(claude-code memory path)/reports/`.

## Shared tree

Every brief says: never `git stash` / `checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); check writes with `git diff --stat`; stage named files only.

## Rules

- **Visible dispatch (owner preference from 2026-11-12, until lifted):** every dispatch goes through the Agent tool
  with a harness type (`claude`, `codex`, `copilot`, `gemini`, `opencode`); the main session does not run
  `claude-code agents ask --background` from Bash. Routing rides the brief's leading `Run:` line. A job that can
  outlast a Bash call (~10 minutes) puts `--background` on that line: the stop gate holds the leaf until the job ends,
  whereas a shell-backgrounded ask dies when the leaf stops. Cap: `rules/harness/hooks.md` → (dispatch cap); queue the
  rest.
- Decide the owner before gathering evidence, never redoing its searches. Simplify runs once per diff, by the manager
  (`rules/standards/quality.md` → Verification Order step 5); detectors (`claude-code review *`) once per pipeline.
- Preserve explicit agent/model choices unless the owner authorizes a change; models go on `ask --model`.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Retry once with its failure output and a changed brief; a second failure: land the rest, name the gap |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Timeout | Keep partial results, cancel the rest |
| A leaf touched a file it didn't own | Revert that hunk, re-dispatch with `Owns:` sharpened |
| External agent unavailable (credential or route missing, e.g. no M2M secret) | Report BLOCKED with the fix command; never silently do it in-context |
| Quota pressure | Dispatches keep their model; route the work to another agent yourself ([cost-routing.md](references/cost-routing.md)) |

## Test scenarios

- **Normal**: two `claude` leaves on disjoint packages plus a `copilot` test agent in one message; fan-in reads each
  diff and the seam, the test agent runs on "landed", simplify runs once, one commit names the files.
- **Error**: a leaf fails `Verify:` twice (second time with failure output and a changed brief); the others land, the
  report names the gap, nothing is silently redone in-context.
