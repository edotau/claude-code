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
| `pi` | Review, exploration, audits and self-tested edits on any Databricks model | Cited findings |
| `Explore` | Scoped read-only scan before non-trivial work | Codebase Context Brief |
| `Plan` | Read-only design on the session's model unless the call names one | Plan in `docs/templates/plan.md` form |
| `general-purpose` | Built-in work when no named role fits | Brief's requested report |
| `web-fetch` | Host built-in page fetch: a URL and a question; the hook changes nothing | The answer |
| `claude-code-guide` | Host built-in: questions about Claude Code, the Agent SDK and the Claude API | The answer |
| `statusline-setup` | Host built-in: configures the status line setting | The settings edit |

Build every `Run:` line with `claude-code agents label <type> --run [--model m] [--profile p | --spec FILE] --task
FILE`: it prints the exact `Run:` line, then the label (`--session-model` supplies the parent's). Pass the Agent
tool's `model` to raise one call (`fable` only for two large designs at once). Wrapper
frontmatter controls the relay, not the vendor model. `dispatch-effort` sets model and effort only for an agent with no
`model:` line and a call naming none (`Explore`, `general-purpose`, untyped). The root machine's pins and per-call flags
select external execution; never overwrite another machine's pins.

### External relay contract

For `codex`, `copilot`, `gemini`, `opencode` and `pi` wrappers:

- A thin pipe; a brief that starts with `Run:` is a relay job, in this order:
  1. You do none of the work yourself.
  2. Write everything after the `Run:` line, unchanged, to the path `claude-code agents brief --path <agent>` prints
     (Read it first: Write fails on an unread file). Never choose or reuse a path; no `$`, `~` or glob.
  3. Run the `Run:` line once, flags and model unchanged, with the brief attached as `- < PATH` (`agents ask -h`: no
     task or `-` reads stdin; a bare run prints usage, exit 2). Never add, drop or swap a flag (an "edit nothing"
     brief keeps `--tools`) or start another ask.
  4. If it prints usage, fails to start or the vendor refuses, do NOT do the work: hand back the exact command, its
     output and exit code.
  5. Always end with `SubagentHandback`, even on failure.
- Without a `Run:`, pick one supported verb (a bare task means `ask`), default a named-agent task to `--fallback
  none`, keep explicit policy and pins, and attach the task the same way.
- Background only via the `Run:` line's `--background` (never the shell or `run_in_background`). No duplicate ask,
  readiness smoke or repeat discovery.
- **Waiting and handing back** (hooks back these up: `rules/harness/hooks.md` → `dispatch-handback`):
  1. A job over ~9 min outlives the Bash tool (hard max 600000 ms), and a foreground ask the Bash tool backgrounds
     prints no id, so the hand-back hook refuses with "No exact dispatch id" for up to 2 h, unless your own successful
     `agents result|wait <id>` names a record of your session whose agent and prompt match your own ask. A long task's
     `Run:` line therefore carries `--background`; read the id from its `job <id> started` line. Run an ask with
     `"timeout": 600000`, once, never re-run it.
  2. Wait with ONE call per turn: `claude-code agents result -f <id>` (`-f` follows to the end; or `agents wait <id>
     --timeout 590s`, exit 4 = still running), 600000 ms Bash timeout. Never poll with Read, `head`, `ls`, `--verdict`
     or `agents ls`, never sleep, start a second poller or use Monitor.
  3. The moment it returns with the job ended, hand back its result as your next action.
  4. After 4 waits (40 min) with the job still running, call `SubagentHandback` at once with the dispatch id, the
     command `claude-code agents result -f <id>` and the words "job still running".
  5. A message from the manager that arrived while the job ran goes in the hand-back under "Open items (not delivered
     to the job)". Do not hold it.
  6. After handing back, stop. If a later notification mentions the same job, reply "already reported" and do not
     poll again.
- The final answer always goes through `SubagentHandback`, never plain text. A refusal naming a missing dispatch id
  is not fixed by retrying: after the second one, stop and end with the id and `agents result -f <id>` in your text.
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
| A plan's waves, observed | `claude-code agents ask --waves <plan>` (a flag, not a verb; never commits) ; exit codes, rerun and the `--background` + `agents wait` loop: [agent-verbs.md](references/agent-verbs.md) |
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
  identifies a plan task for the hook's slot claim. `Tier: fast|standard|deep` (own line) sets the task tier that
  picks an unpinned run's model (`agent.TaskTier`); without it the wording decides. Never hand-type the line: take it
  from `agents label --run`; `dispatch-effort` fixes a typo it can decide and notes the rest. Name the plan task
  (`Task: Phase W.k`): the hand-back then carries its `verify:` result.
- `Owns:` absolute paths and forbidden files. `Method:` the skill. `Context:` evidence, decisions, plan path.
  `Constraints:` applicable rules and the [Shared tree](#shared-tree) line. `Verify:` a scoped command;
  `Acceptance:` its observable outcome. `Workflow:` a saved Workflow, only when the owner requested it.
- Ask the leaf to list the files it actually changed in its report.
- State what to report on a missing credential, a red gate or a file the leaf doesn't own, and any dependency on
  another leaf.
- `claude-code agents brief --task F --own P…` (or `--read-only`) prints the brief: `Owned:`, mode, your task, the leaf
  rules and the reporting contract from `internal/agent/brief.tmpl`; never restate that footer. Pipe it to
  `agents ask -`; `agents diff --verify <id>` reads `Owned:` back.

The hook reads `Agent:` only before the first blank line. A leading `Run: claude-code agents ask …` or
`claude-code <agent> ask …` (optionally with global `--remote <h>`) is left unrewritten; `--profile` leads then
falls back unless `--fallback none` pins it; `Run:` lines and held-job hints carry the route as `--overlay '<object>'`
(one unit: provider and profile together, an inherited `COWORKER_OVERLAY` is a fence). Overlay keys:
`rules/harness/agent.md` → Dispatch behavior; auto-mode rewrites: `rules/harness/hooks.md`;
`--remote` vs `--host`: `rules/harness/network.md`.

**Report (out)** — the final answer is a verdict of at most 8 lines: a review's verdict word,
else one Status: `DONE` · `DONE_WITH_CONCERNS` (resolve before review) · `NEEDS_CONTEXT`
([Return to owner](#return-to-owner)) · `BLOCKED` (change something; never resend unchanged); then `Ran on:` (the
`ask: ran on …` line; `self` only for `claude`, `Explore`, `Plan` or an `Agent: in-context` brief), outcome per item,
test or gate results, what needs a decision, and the report path. The full report (root cause, before/after output,
numstat, teeth proof, commands) goes to `$(claude-code memory path reports YYYY-MM-DD-<name>.md)`, wrapped
at 118 columns. Scratch files only under an absolute `/tmp/<job>/` path, removed before reporting.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface, the manager making the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit or run the suite (an `ask` is allowed).

### Phase 2: dispatch

Independent leaves go in one message as Agent calls with `run_in_background` (the manager's; a relay never uses it).

#### Concurrency

Cap: `rules/harness/hooks.md` → (dispatch cap); repair leaves count. Batch deep-context leaves (Databricks `429`) and
fleet scans.

Run at capacity: keep every unblocked task dispatched, and refill as soon as a leaf hands back or a file frees up;
an idle slot with unblocked work is a fault. Spread leaves over the three Databricks profiles (`--profile
dev|qa|prod` on the relay's Run line) and the four vendor CLIs so no single account carries the wave. The only
limits are overlapping files, dependency order and an account's own rate limit.

#### Wave loop

The main thread plans waves, dispatches, decides and commits; file reads, diff review, gate runs and review fixes go to
leaves that hand back a short verdict, so its context stays small. Run it without prompting:

1. **Queue:** the [Dispatch table](#dispatch-table) holds the blocked tasks; dispatch one the moment its blocker
   frees, and again after every hand-back, until none remain or what is left needs the owner.
2. **Per hand-back:** a review leaf returns PASS/FAIL, ranked findings and the exact files to stage (it measures and
   reads the diff, not the manager) → a fix leaf for each finding → the manager re-runs the scoped gate and commits
   named files (`rules/standards/worktree-git.md`) → dispatch what that unblocked. One `rtk make audit`,
   `rtk make install` and push per green wave (`CLAUDE.md` hard rule 5).
3. **Bugs:** any bug or issue a leaf or review reports gets a fix leaf at once, without asking.
4. **Open items:** pick the recommended option, apply it and tag `DECISION:`; ask only for frozen hook code,
   `settings.json`, secrets or an irreversible outward action.
5. **Briefs that do not stall:** say what is already verified (never "survey, then stop"), name a hand-back line cap,
   and own each source file's `_test.go` (a test that must sit in another package's test file to avoid an import cycle
   is allowed); a leaf stops only for a production file another agent owns.

#### Dispatch table

Write it before the first dispatch of a multi-part request, re-check it on every hand-back and every new user message
(merge the new asks into it; never serve requests in arrival order), and dispatch every unblocked row in one message.

| Task | Owns | Blocked by | Agent |
|---|---|---|---|
| A: fix flag parse | `/r/internal/cli/args.go` | none | claude |
| B: fix flag help | `/r/internal/cli/args.go` | A (lane 2 of 2) | claude (lane of A) |
| C: add retry | `/r/internal/net/retry.go` | none | copilot |
| D: review A | read-only | A | codex |

- **Hot file:** when 2+ queued tasks own one file, first dispatch a split (extract by function into a new file in the
  same package, behavior unchanged) so they run in parallel; if not worth it, show that file's queue and length.
- **Lane:** `SendMessage` / `claude-code agents send <id> <task>` is for work in files that leaf already owns; show
  the lane's length, and past 2 queued items start a second leaf on a different file instead of appending a third.
- **Know what leaves hold:** ask, never assume the brief: every brief has the leaf list the files actually changed in
  its report; while it runs, `rtk git status --short` and the `worktree-claim` files (`rules/harness/hooks.md`)
  show what is dirty, and `claude-code agents tree` / `agents status` show who is live.
- **Review per finished file or package** as each implementer hands back, not after all of them.
- **Gates lock the tree, not the dispatcher:** during a timed `make audit`, an install or a live check, keep
  dispatching read-only work and work outside the tree (scratch dirs, a linked worktree); queue only the commits.
- **Lanes:** with `HARNESS_WORKTREE=lanes` each stream's leaves work in their own lane and the main session lands it
  with `claude-code worktree merge <lane>` (rebase in the lane, gate, fast-forward; a conflict exits 3):
  `skills/using-git-worktrees/SKILL.md` → The lane protocol.
- **Read-only scouts and reviews** never wait for edit work.

#### Tests

The implementer writes the basic tests for its own feature (R15); other cases are proposals in its hand-back. Broken
existing tests are repaired by the implementer or one repair leaf (`copilot` or `claude`). The main thread owns the
live check. Loop: `rules/standards/testing-guidelines.md`. A leaf that fails one test 3 times reports and stops
(`rules/standards/testing-guidelines.md` → When a test fails).

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one `copilot` leaf |
| Failures across packages | one `copilot` leaf per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | the implementer writes its basic tests; a repair leaf only for tests the change broke |
| A fixed bug | after your live before/after, the regression test is a proposal the owner approves |
| Suite must go green, bounded | `Workflow code-looper` — only when the user asks |

### Phase 3: integrate

#### Fan-in

Treat every report as a claim, verified by a review leaf (diff, deleted blocks via callers and `git log -S`, edits
outside the leaf's files, a new test that can fail); the manager reads its verdict, re-runs the scoped check and
rebuilds before trusting a binary ([Wave loop](#wave-loop)).
Leaves hand back uncommitted unless the brief says to commit; main verifies, then `worktree commit --task` ticks.

- Parallel leaves that built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔
  brief writer): read both together; `rtk go build ./...` misses a broken consumer.
- Disagreeing reviewers: keep both findings with sources. Simplify once over the merged diff, then review.
- **Saved workflow output** is a claim too, and a workflow's run has its own rules:
  [saved-workflows.md](references/saved-workflows.md)
  → Fan-in and run rules.

### Phase 4: finish

Checks: `rules/standards/quality.md`; commits only when authorized: `rules/standards/worktree-git.md`.

#### Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or
`sleep` yourself: a long `ask` runs `--background`, then foreground `result -f <id>` (600000 ms, rerun only on a
timeout, at most 12 times;
[agent-verbs.md](references/agent-verbs.md) step 2). Re-dispatch only when the owner is gone; after a restart read
`git status`; never relaunch a leaf the user stopped. Reports go to
`$(claude-code memory path reports YYYY-MM-DD-<name>.md)`.

## Shared tree

The main shell stays at the repo root (use absolute paths, never a bare `cd`). Every brief says: never `git stash` /
`checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); run `claude-code agents diff --check --path <files>`, fix your own files
(`agents fix --path <files>`), never `make fmt`, `audit` or `install`; edit files with Edit or Write, never a
`python3 -` script (`safety-scan` asks on interpreter writes); check writes with `rtk git diff --stat`; stage named
files only.

## Rules

- **Visible dispatch (owner preference from 2026-11-12; lifted for long jobs on 2026-11-20):** a dispatch goes through
  the Agent tool with a harness type (`claude`, `codex`, `copilot`, `gemini`, `opencode`, `pi`). A job expected to
  outlast a Bash call (~10 minutes) may instead be started by the main session itself with
  `claude-code agents ask --background`, then `claude-code agents wait <id>... --timeout 9m` and `agents result <id>`:
  no leaf sits waiting on a shell. Routing rides the brief's leading `Run:` line. A relayed job that can
  outlast a Bash call (~10 minutes) puts `--background` on that line: the stop gate holds the leaf until the job ends
  (≤ 2 h; only dispatch ids its own `ask`, `dispatch` or `result` call printed count, plus an id its own successful
  `result`/`wait` call named whose record is this session's and matches that leaf's ask by agent and prompt),
  whereas a shell-backgrounded ask dies when the leaf stops. A hand-back waits up to 13 s for a running job, then is
  held; a hold keeps every finished job's answer and the later pass attaches them in dispatch order. A never-run
  `Run:` line is run by the hook, which tells the leaf to wait. Cap: `rules/harness/hooks.md` → (dispatch cap); queue
  the rest.
- Decide the owner before gathering evidence, never redoing its searches. Simplify runs once per diff, by the manager
  (`rules/standards/quality.md` → Verification Order step 5); detectors (`claude-code review *`) once per pipeline.
- Preserve explicit agent/model choices unless the owner authorizes a change; models go on `ask --model`.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Retry once on a `claude` leaf with its failure output; a second failure: the main session finishes it where it can and says so, else lands the rest and names the gap |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Timeout | Keep partial results, cancel the rest |
| A leaf touched a file it didn't own | Revert that hunk, re-dispatch with `Owns:` sharpened |
| External agent unavailable (credential or route missing, e.g. no M2M secret) | Report BLOCKED with the fix command; never silently do it in-context |
| Quota pressure | Dispatches keep their model; route the work to another agent yourself ([cost-routing.md](references/cost-routing.md)) |

## Test scenarios

- **Normal**: two `claude` leaves on disjoint packages in one message; fan-in reads each
  diff and the seam, simplify runs once, one commit names the files and task
  (`worktree commit --task <id>`; repeat for multiple ids).
  Use `--plan <file>` when ids collide. The trailer `Task: <plan-slug> <id>` survives rebase and cherry-pick;
  a named task ticks once its commit is on main, however many `owns:` files it covers; name it on its last commit.
  Only main ticks, after verifying a hand-back; leaves hand back uncommitted unless the brief says to commit.
  Coverage never ticks, it prints one candidate; `plan task <id> --done --sha <sha> --plan <file>`
  is the manual route.
- **Error**: a leaf fails `Verify:` twice (second time with failure output and a changed brief); the others land, the
  report names the gap, nothing is silently redone in-context.
