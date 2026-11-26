---
name: subagent-workflows
description: >-
  Run multi-agent work end to end, from picking the shape (fan-out on disjoint files, sequential plan, lane
  orchestrator, tournament, saved Workflow), write briefs, dispatch, collect and hand back. Use for "fan this out",
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
| Workflow | the script | the steps are known up front and must run the same every time |

Workflow requested, none fits: run `code-engineer` (`~/.claude/workflows/code-engineer.js`).

## Leaf types

Shell `agents ask` belongs in a carried dispatch, workflow stage or requested script. Owner shortcuts:
`rules/workflows/agents.md` → Slash commands.

| Leaf | Role |
| --- | --- |
| `claude` | Code leaf: Pi first; Sonnet in-context on Pi refusal. |
| `Explore` | Scoped scan returning a Codebase Context Brief. |
| `Plan` | Design on the selected model, in `docs/templates/plan.md` form. |
| `general-purpose` | Built-in work when no named role fits. |
| `web-fetch` | Host page fetch, where available; otherwise `general-purpose`. |
| `codex`, `copilot`, `gemini`, `opencode` | External work through a Haiku relay running its prescribed ask. |
| `coworker` | Pi in code mode through a Haiku relay; raw tool output, rtk off. |
| `claude-code-guide` | Host guidance on Claude Code, Agent SDK and Claude API. |
| `statusline-setup` | Host status-line configuration. |

Relays report the inner `ask: ran on` receipt and verdict; their Haiku wrapper is not the work model.
Their agent files own the prescribed route; Gemini uses its own pin.

Print a leaf's description prefix with `claude-code agents label <type> [--model m] [--profile p] --task FILE`
(`--session-model` supplies the parent's). For a relay type it prints the route's predicted work model (`rotation
next …` on stderr) while the host row and receipt show the Haiku relay; only the inner `ask: ran on` line names the
model that did the work. Pass the Agent tool's `model` to raise one call. Model by job: fix and review-loop leaves run
Sonnet (the review itself stays another family, R6), mechanical fixes `Tier: fast` with overlay `"model":"auto"`
(R24; Haiku on a Claude leaf); Opus or higher only for planning, security and guard design, architecture, and
judgment calls (benchmark verdicts, schedule, retry, keep/revert); `fable` only for two large designs at once, never
inherited from the session; a deterministic step uses no model. The root machine's pins and per-call flags select
external execution; never overwrite another machine's pins.

### Carried dispatch (main session, no relay)

For an agent whose row sets `Carry` (codex, copilot, gemini, opencode), run the ask yourself as a background Bash
task, then follow it with one wait call; `--carry` refuses pi and claude and takes no `--dry-run` or `--env` (preview
by dropping it). Pi runs the same way without `--carry`, on any Databricks model for review, audits and self-tested
edits. As an Agent leaf instead, pick the type (`agents/<type>.md`); its own CLI on its own login stays reachable here
(`claude-code <agent> …`, overlay `{"runner":"cli"}`). Codex handles cross-family review, plan vetting and delegated
edits (`claude-code codex review` for a diff), gemini research and codebase sweeps, opencode code and docs work,
copilot test repair and mechanical edits:

```bash
claude-code agents ask --carry --background --overlay '{"agent":"<agent>"}' [ask flags] - < BRIEF
claude-code agents result -f <id>
```

The carrier runs the inner ask as a plain child (no model tokens); a cut-off child is finished by Pi, and the receipt
ends `ask: finished by pi (<model>)`. Rules: `rules/harness/agent.md` → Pi carrier and experiments.
`agents wait --progress` shows a Pi leaf's `compacting` or `retrying` phase instead of `quiet`; the idle limit applies.
**Default leaf route:** a `claude` code leaf whose brief has no `Agent:` line runs the task as a Pi ask first; review,
docs and gate leaves stay in-context. A brief naming `Agent: pi` (or the type `pi` or `code-mode`) is retyped to
`coworker`; with `Codemode: off` it stays a `claude` leaf running a plain Pi ask. A Pi refusal or failed
start (not on the profile's menu, no Databricks route, guard did not load) is the one refusal below: it does the task
in-context, `Ran on:` naming the failed Pi attempt and what then ran. A brief needing the Agent tool, skills or MCP
says `Agent: in-context`; `Agent: claude` in a wave plan runs a headless claude ask.
Copilot repair briefs name `rules/standards/testing-guidelines.md` → TEST and use overlay `"tools":true`.

### External relay contract

Run the prescribed ask once, using [agent-verbs.md](references/agent-verbs.md) for syntax.

- Reserve the brief with `claude-code agents brief --path <agent>`, read it, then attach the task at that exact path.
  Keep the prescribed route and flags; add `--workdir` only for the brief's `Workdir:`.
- Use `agents ask --background` for background work, never shell backgrounding or `run_in_background`.
- On usage, startup failure or vendor refusal, return the command, redacted error and job id; do not silently
  substitute an agent. The default Pi refusal in Carried dispatch is the exception.
- Wait with one `claude-code agents result -f <id>` or `agents wait <id> --timeout 590s` per turn.
  Never start another poller, sleep, or poll files, status or verdicts. After four waits with a running job,
  hand back its id and follow command as "job still running".
- When the job ends, make `SubagentHandback` the next action. Include manager messages received meanwhile under
  "Open items (not delivered to the job)". Then stop; later notifications get "already reported".
- Return the work agent's verdict under `## Result`, preceded by its actual `ask: ran on` receipt and job/session
  ids. Add `Relay note:` for dispatch errors; never manufacture a receipt. An unattributed ask is unproven.
- Run only the stage's prescribed `agents diff --verify <id>`; fan-in belongs to the manager.
- Commit authorization and relay limits: `rules/standards/worktree-git.md` → Worktrees.
- Inside the vendor CLI or Pi, do the task directly; never invoke the wrapper or ask yourself.

Receipt and stop enforcement: [hook-details.md](references/hook-details.md) →
dispatch-failure · dispatch-workflow · dispatch-stop · dispatch-handback.

## Workflow

### Phase 0: intake and triage

The tier in `rules/workflows/master-workflow.md` → Phase 0 picks the shape; a one-line edit runs inline. Then check
context: follow-up (re-run one leaf with its prior report and `git diff`) or continued plan (resume at the first
unticked line of its `~/.claude/plans/` checklist).

| Shape | Do |
| --- | --- |
| Single leaf | One `Agent` call with a brief ([prompts.md](references/prompts.md)) |
| Parallel fan-out | `Agent` calls in one message, disjoint files ([Ownership](#handoff-contract)) |
| Which agent, model, workspace | [agents-and-models.md](references/agents-and-models.md) |
| Same question, many angles | Separate leaves; routes: [cost-routing.md](references/cost-routing.md). |
| Sequential plan · tournament | [patterns.md](references/patterns.md) |
| A plan's waves, observed | `agents ask --waves`; lifecycle: [agent-verbs.md](references/agent-verbs.md). |
| Many leaf-sized tasks | [Lane orchestrator](references/lane-orchestrator.md) |
| Saved `Workflow` | [saved-workflows.md](references/saved-workflows.md) and its composite templates. |
| Fleet scan | A carried `copilot` ask with an explicitly requested fleet task; verify the returned diff |

### Phase 1: prepare

Give each leaf named files it owns (never shared between parallel leaves) and fan out as many leaves as the task
allows (`CLAUDE.md` guideline 3): split until files would overlap or one leaf must wait on another's result.

#### Handoff contract

**Brief (in)** — a fresh agent can act from it alone (the CLI starts cold and cannot see this session):

- `Objective:` one outcome. `Agent:` the work agent; omit for the selected type, `in-context` for direct work; keep
  it consistent with the type (no hidden relay).
- `Model:` is not parsed by the hook: put overlay `model` on the carried ask
  (`agents ask --carry --overlay '{"agent":"<a>","model":"m"}'`).
  `Mode:`, `Owns:`, `Method:`, `Verify:`, `Acceptance:` are conventions, not routing hooks. `Task:` identifies a plan
  task for the hook's slot claim (`Task: Phase W.k`): the hand-back then carries its `verify:` result. `Tier:
  fast|standard|deep` (own line) sets the task tier that picks an unpinned run's model (`agent.TaskTier`); without it
  the wording decides. A gate-run leaf that only reports carries `Tier: fast`.
- `Owns:` absolute paths and forbidden files; the report lists the files actually changed. `Method:` the skill.
  `Context:` evidence, decisions, plan path. `Constraints:` applicable rules and the [Shared tree](#shared-tree) line.
  `Verify:` a scoped command; `Acceptance:` its observable outcome. `Workflow:` a saved Workflow, only when the owner
  requested it.
- A type with no agent file (`general-purpose`, `Explore`) gets no leaf rules: paste the `## Leaf rules` lines of
  `internal/assets/brief/brief.tmpl` (no foreground `sleep`, no git or `sudo` probes in `~/.claude`) into its
  brief.
- State what to report on a missing credential, a red gate, an unowned file or a dependency.
  Scope the brief to one outcome and name the evidence needed.
- `claude-code agents brief --task F --own P…` prints the brief: `Owned:`, mode, your task, the leaf
  rules and the reporting contract from `internal/assets/brief/brief.tmpl`; never restate that footer. Pipe it to
  `agents ask -`; `agents diff --verify <id>` reads `Owned:` back.

The hook reads `Agent:` only before the first blank line. Overlay `profile` leads then falls back unless
`"fallback":"none"` pins it; carried asks carry the route as `--overlay '<object>'`
(one unit: provider and profile together, an inherited `COWORKER_OVERLAY` carries a child's derived confinement).
Overlay keys and inheritance: `rules/harness/agent.md` → Overlay; auto-mode rewrites: `rules/harness/hooks.md`;
`--remote` vs `--host`: `rules/harness/network.md`.

**Report (out)** — the final answer is a verdict of at most 8 lines: a review's verdict word,
else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve before review) · `NEEDS_CONTEXT`
([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend unchanged); then `Ran on:` (the
`ask: ran on …` line; `self` only for `claude`, `Explore`, `Plan` or an `Agent: in-context` brief), outcome per item,
test or gate results, what needs a decision, and the report path. The full report (root cause, before/after output,
numstat, teeth proof, commands) goes to `$(claude-code memory path reports YYYY-MM-DD-<name>.md)`.
Scratch files only under an absolute `/tmp/<job>/` path, removed before reporting.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface; the manager makes the integration edit and owns decisions.
Leaves remain terminal (an external `ask` is allowed). Commit authority: `rules/standards/worktree-git.md` → Worktrees.

### Phase 2: dispatch

Independent leaves go in one message as Agent calls with `run_in_background` (the manager's; a relay never uses it).

#### Concurrency

Cap: `rules/harness/hooks.md` → (dispatch cap); repair leaves count. Batch deep-context leaves (Databricks `429`) and
fleet scans.

Run at capacity: keep every unblocked task dispatched and refill as a leaf hands back or a file frees up; an idle slot
with unblocked work is a fault. Spread leaves over the three Databricks profiles (R20;
overlay `"provider":"databricks","profile":"<p>"` picks
one, `"profile":"dev|qa|prod"` names it) and the four vendor CLIs so no single account carries the wave. The only limits
are overlapping files, dependency order and an account's own rate limit.

#### Dispatch workflow (R32)

Tree selection and collection: `rules/standards/worktree-git.md` → Worktrees.
Validation: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
Pi or an external agent implements and fixes; Claude verifies cheap checks; main integrates. The contract's Opus owner
runs the fan-out; otherwise main. Resolve the reviewer's actual model family, using a typed model when auto routing
could choose the author's family. Review findings and repeat rounds: `rules/standards/quality.md` → Completion.

##### Owner layer (the contract)

The `contract` key of `agents ask --brief` describes a unit, its budget and owner/fleet role:
`unit`, `kind: function|test-repair|file|task`, `limits: {tokens, leaves}`, `role: owner|fleet`, `parent`.
`--dry-run` prints its terms. Go records observed gates, tests, hand-back and reports, plus lane and plan events;
scoped-proxy terms are not wired. The owner judges the evidence; an eyeball check does not replace executed checks.
A fleet leaf stays terminal; use the validation and commit rules linked in Dispatch workflow.

#### Wave loop

Main plans waves, dispatches, decides and integrates.

1. Dispatch each row when its dependencies and owned files are free; refill after every hand-back.
2. Collect completed work through `rules/standards/worktree-git.md` → Worktrees; fan-in does not start a lane review.
3. Fix reported defects within the owner's priority; main handles what a leaf could not fix (R25, R28, R34).
4. Act on routine decisions and record `DECISION:`; approval boundaries remain in `rules/standards/principles.md`.
5. Give briefs verified context, a line cap and the matching test files; another package's test file is allowed
   when needed to avoid an import cycle. A leaf stops for production files another agent owns.

#### Dispatch table

Write it before the first dispatch of a multi-part request (after its dated plan, R18), re-check it on every hand-back
and every new user message (merge the new asks into it; never serve requests in arrival order), and dispatch every
unblocked row in one message.

| Task | Owns | Blocked by | Agent |
| --- | --- | --- | --- |
| A: fix flag parse | `/r/internal/cli/args.go` | none | claude |
| B: fix flag help | `/r/internal/cli/args.go` | A (lane 2 of 2) | claude (lane of A) |
| C: add retry | `/r/internal/net/retry.go` | none | copilot (carried) |
| D: review merged diff | collector files | all implementation lanes collected | other-family reviewer |

- **Hot file:** when 2+ queued tasks own one file, first dispatch a split (extract by function into a new file in the
  same package, behavior unchanged) so they run in parallel; if not worth it, show that file's queue and length.
- **Lane:** `SendMessage` / `claude-code agents send <id> <task>` is for work in files that leaf already owns; show
  the lane's length, and past 2 queued items start a second leaf on a different file instead of appending a third.
- **Know what leaves hold:** ask, never assume the brief: every brief has the leaf list the files actually changed in
  its report; while it runs, `rtk git status --short`, the `worktree-claim` files (`rules/harness/hooks.md`) and
  `claude-code agents tree --files` show what is dirty and who holds it, `agents status` who is live.
- **Gates lock the tree, not the dispatcher:** during a timed `make audit`, an install or a live check, keep
  dispatching reads and work outside the tree (scratch dirs, a linked worktree); queue only the commits.
- **Lanes and landing:** [Landing](#landing).
- **Scouts** can run beside edit work; final review follows Dispatch workflow.

#### Tests

Test selection, live evidence and repair scope: `rules/standards/testing-guidelines.md`.
Use the implementer for small repairs or a carried `copilot` ask with the TEST section and exact failing names.
For independent failures, dispatch one repair leaf per disjoint set of test files. Main owns the live check.
Use `Workflow code-looper` only when requested; its scope follows [saved-workflows.md](references/saved-workflows.md).

### Phase 3: integrate

#### Fan-in

Read hand-backs for completion, owned-file changes and unresolved gaps; collect through the Git rule.
Treat reports as claims. At merged review, check the diff, removed code's callers, ownership and seams between
producers and consumers; a successful build alone does not prove a JSON shape or flag/argv contract.
Simplify once over the merged diff before review. Keep disagreeing findings with their evidence.
Saved workflow results follow [saved-workflows.md](references/saved-workflows.md) → Fan-in and run rules.

#### Landing

Follow `rules/standards/worktree-git.md` → Worktrees; commands and recovery:
`skills/using-git-worktrees/SKILL.md` → The lane protocol.

### Phase 4: finish

Checks: `rules/standards/quality.md`; commits only when authorized: `rules/standards/worktree-git.md`.

#### Return to owner

Send a gap to its still-running owner with the evidence needed and stop condition. Never relaunch a leaf the user
stopped or resend a budget-ended task; main finishes partial work (R34). Background waiting and receipts:
External relay contract. Report placement and format: Handoff contract.

#### Plan upkeep

Plans and task completion: `rules/standards/standing-requirements.md` → R1 and R2.
Record checked results without ticking a commit-dependent task when commits are held.

## Shared tree

Keep the main shell at the repo root and use absolute paths. Shared edits and coordination:
`skills/using-git-worktrees/references/shared-checkout.md`. Cheap validation:
`rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
Fix only owned files with `agents fix --path <files>`; confirm writes with `rtk git diff --stat`.

## Unit files (worktrees only)

Inside a linked worktree, split work into units, not files, so leaves that touch one file still run in parallel.
Everything converges in the worktree before it merges onto the source branch; the live checkout never sees a unit file.
Tree selection: `rules/standards/worktree-git.md` → Worktrees.

- **New code goes in a unit file.** A leaf adding functions or tests writes them to its own file beside the target:
  `<file>_<task>.go`, `<file>_<task>_test.go` (Go compiles a package's files together, so it builds and tests at once;
  elsewhere, new test files plus the fold step). The dispatch table lists units, each owning its unit file.
- **Edits to existing code** still own the real file, one leaf per file, as today.
- **Fold before merge.** One fold leaf moves each unit into its home file (functions beside their callers, tests into
  the `_test.go` matching their source), deletes the unit files, and resolves two units changing the same code as a
  branch conflict, keeping both intents; give it every brief, not only the diffs. Collector validation:
  `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
- **Never commit a unit file.** It breaks the one-test-file-per-source rule (`rules/harness/go.md`); the gate catches
  one left behind.
- A test leaf proves teeth on a scratch copy (`go test -overlay`), never by editing source an editor leaf owns.

## Rules

- Use the Agent tool with a harness type, or a carried ask from main; long jobs use the External relay contract.
  Cap: `rules/harness/hooks.md` → (dispatch cap); queue the rest.
- Decide the owner before gathering evidence, never redoing its searches. Simplify runs once per diff, by the manager
  (`rules/standards/quality.md` → Verification Order step 5); detectors (`claude-code review *`) once per pipeline.
- Preserve explicit agent/model choices unless the owner authorizes a change; models go on
  `ask --overlay '{"model":"<m>"}'`.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Use R34: main takes over after two failed attempts or broken dispatch. |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Budget ended | Keep partial work; main finishes it without resending (R34). |
| Unowned edit | Report it; remove only hunks proven to be the leaf's, preserving pre-existing edits. |
| Block on a leaf | Report the refusal; guard policy: `rules/harness/hooks.md` → `safety-scan`. |
| Missing credential or route | Report BLOCKED with the fix command; no silent in-context substitution. |
| Quota pressure | Preserve model choice; fallback: [cost-routing.md](references/cost-routing.md). |

## Test scenarios

- **Normal**: dispatch disjoint leaves, collect each hand-back, then follow the collector sign-off.
- **Error**: main finishes a failed leaf's partial work before sign-off; report an unresolved blocker.
