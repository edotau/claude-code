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
| Workflow | the script | the steps are known up front and must run the same every time |

Workflow requested, none fits: run `code-engineer` (`~/.claude/workflows/code-engineer.js`).

## Leaf types

Shell `agents ask` belongs in a carried dispatch, workflow stage or requested script. Owner shortcuts:
`rules/workflows/agents.md` → Slash commands.

| Leaf | Role | Output |
| --- | --- | --- |
| `claude` | Code leaf with no `Agent:` line: runs as a Pi ask first, in-context on Sonnet when Pi refuses; a typed `claude` ask on a Databricks route also runs through the pi runner first (overlay `"runner":"pi"` or `"own"`, Databricks default); dispatches a step it needs another agent for | Status and verification |
| `Explore` | Scoped scan before non-trivial work | Codebase Context Brief |
| `Plan` | Design on the session's model unless the call names one | Plan in `docs/templates/plan.md` form |
| `general-purpose` | Built-in work when no named role fits | Brief's requested report |
| `web-fetch` | Host built-in page fetch, only where the host offers it (else `general-purpose`) | The answer |
| `codex` · `copilot` · `gemini` · `opencode` | External work as an Agent leaf (a relay): a Haiku worker runs the one typed `agents ask --overlay '{"agent":"<type>"}'` its file bakes (overlay `"provider":"databricks","profile":"<p>"`, gemini excepted: agy on its own pin), waits, checks the diff and verify command; the inner `ask: ran on` line names the model that did the work | `ask: ran on` line, then the verdict |
| `coworker` | Pi in code mode, script-style work that needs raw tool output (a relay): a Haiku worker runs the typed `agents ask --overlay '{"agent":"pi","provider":"databricks","profile":"<p>","codemode":"on","tools":true}'` its file bakes; rtk off; receipt `coworker` | `ask: ran on` line, then the verdict |
| `claude-code-guide` | Host built-in: questions about Claude Code, the Agent SDK and the Claude API | The answer |
| `statusline-setup` | Host built-in: configures the status line setting | The settings edit |

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
(`claude-code <agent> …`, overlay `{"runner":"own"}`). Codex handles cross-family review, plan vetting and delegated
edits (`claude-code codex review` for a diff), gemini research and codebase sweeps, opencode code and docs work,
copilot test repair, mechanical edits and authorized LAST-MILE:

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
Copilot repair briefs name `rules/standards/testing-guidelines.md` → TEST; authorized landing briefs name
`rules/standards/worktree-git.md` → LAST-MILE. Both need overlay `"tools":true`.

### External relay contract

For a `claude` leaf that dispatches a step of its own task (`agents/claude.md` → Work here or dispatch), or a routed
`claude` workflow stage; the wait, hand-back and receipt rules bind both:

- Run the prescribed ask once: `claude-code agents ask --carry --overlay '{"agent":"<a>"}' …`, the plain ask for
  `claude`/`pi`, or, for a restored type (`agents/codex.md`, `copilot.md`, `gemini.md`, `opencode.md`, `coworker.md`),
  the one non-carry typed `agents ask --background --overlay '{"agent":"<type>","tools":true}' - < PATH` its file
  prints (Databricks baked, gemini excepted; pi adds overlay `"codemode":"on"`); copy it as printed, add
  `--workdir <dir>` only for a `Workdir:` line, and never add overlay `profile` or `model`:
  1. Write the task to the path `claude-code agents brief --path <agent>` prints (Read it first), and attach it as
     `- < PATH` (or `--attach <file>`). Never choose or reuse a path; no `$`, `~` or glob. Never add, drop or swap a
     flag or start another ask.
  2. If it prints usage, fails to start or the vendor refuses, do NOT do the work: hand back the exact command, its
     output and exit code (the default Pi route above is the one exception).
  3. Always end with `SubagentHandback`, even on failure.
- Background only via `agents ask --background` (never the shell or `run_in_background`). No duplicate ask,
  readiness smoke or repeat discovery.
- **Waiting and handing back** (hooks back these up: `rules/harness/hooks.md` → `dispatch-handback`):
  1. A job over ~9 min outlives the Bash tool (hard max 600000 ms), so long tasks carry `--background`; read the id
     from its `job <id> started` line. Run an ask with `"timeout": 600000`, once, never re-run it.
  2. Wait with ONE call per turn: `claude-code agents result -f <id>` (`-f` follows to the end; or `agents wait <id>
     --timeout 590s`, exit 4 = still running), 600000 ms Bash timeout. Never poll with Read, `head`, `ls`, `--verdict`
     or `agents status`, never sleep, start a second poller or use Monitor.
  3. The moment it returns with the job ended, hand back its result as your next action.
  4. After 4 waits (40 min) with the job still running, call `SubagentHandback` at once with the dispatch id, the
     command `claude-code agents result -f <id>` and the words "job still running".
  5. A message from the manager that arrived while the job ran goes in the hand-back under "Open items (not delivered
     to the job)". Do not hold it.
  6. After handing back, stop. If a later notification mentions the same job, reply "already reported" and do not
     poll again.
- The final answer always goes through `SubagentHandback`, never plain text. An unattributed note is not fixed by
  retrying: hand back once, with the id and `agents result -f <id>` in your text.
- **A relay type never commits** (`codex`, `copilot`, `gemini`, `opencode`, `coworker`; `isolation: worktree` makes it
  edit in a lane). Its shell is `claude-code` asks, read-only commands and the `go build|vet|test` and `gofmt -l`
  gates, so `git add` is refused (`not a claude-code command`) and so is `worktree commit` (`a relay does not commit;
  the dispatched agent or the main session commits`). It hands back uncommitted; the main session commits and lands
  ([Landing](#landing)). When it names a plan task, the hand-back opens with `verify <task>: …` lines and a
  `commit+tick: [cd <lane> && ]claude-code worktree commit --task '<id>' --plan <file> …` line, and carries a `NOTE:`
  (`the relay's ask could not be attributed to a dispatch record`) when the ask matches no record: treat the work as
  unproven and check the diff.
- Run only the stage's prescribed `agents diff --verify <id>` after the ask; the manager verifies at fan-in.
  Report a dispatch error; never repair it in-context or silently pick another agent
  (one exception: the default route's Pi refusal).
- Return the agent's verdict (Report (out)), not its whole answer, under `## Result`, preceded only by the actual
  `ask: ran on` line and job/session ids, plus a `Relay note:` for a dispatch error. Stop gate:
  `rules/harness/hooks.md` → `dispatch-stop`; never manufacture a receipt.
- Inside the vendor CLI or Pi, do the task directly; never invoke the wrapper or ask yourself.

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
| Same question, many angles | Separate Agent leaves; shell role commands only for requested scripts ([cost-routing.md](references/cost-routing.md)) |
| Sequential plan · tournament | [patterns.md](references/patterns.md) |
| A plan's waves, observed | `claude-code agents ask --waves <plan>` (a flag, not a verb; never commits) ; exit codes, rerun and the `--background` + `agents wait` loop: [agent-verbs.md](references/agent-verbs.md) |
| Many leaf-sized tasks | [Lane orchestrator](references/lane-orchestrator.md) |
| Saved `Workflow` | `workflows/*.js`, **only when the user asks** ([saved-workflows.md](references/saved-workflows.md)) · templates: [composites.md](references/composites.md) |
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
  `internal/assets/files/brief/brief.tmpl` (no foreground `sleep`, no git or `sudo` probes in `~/.claude`) into its
  brief.
- State what to report on a missing credential, a red gate or a file the leaf doesn't own, and any dependency on another
  leaf. Keep the brief scoped: one asking a leaf to verify a whole large doc loops (the opencode run behind this edit
  made 176 shell calls).
- `claude-code agents brief --task F --own P…` prints the brief: `Owned:`, mode, your task, the leaf
  rules and the reporting contract from `internal/assets/files/brief/brief.tmpl`; never restate that footer. Pipe it to
  `agents ask -`; `agents diff --verify <id>` reads `Owned:` back.

The hook reads `Agent:` only before the first blank line. Overlay `profile` leads then falls back unless
`"fallback":"none"` pins it; carried asks and held-job hints carry the route as `--overlay '<object>'`
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
new file and reports its API surface, the manager making the one integration edit. The manager owns decisions,
integration and commits; leaves never dispatch subagents, commit (bar the verifier's lane commit) or run the suite (an
`ask` is allowed); only a contract owner (below) leads a fleet.

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

1. One lane per task: each dispatched code task has its own linked worktree; no two tasks share one.
2. One sign-off gate: `claude-code worktree land <lane>`; a leaf runs no gate chain of its own.
3. One shared review at the end (R28): finished lanes go into the collector unreviewed; its merged diff gets the
   other-family review, every finding fixed, repeated until a review finds nothing; three
   rounds in a row each finding a new HIGH in one area stop the loop and go to the owner. A review dispatch passes a
   typed non-Claude overlay `model` (a GPT or Gemini id) when the reviewer's route is auto, so other-family holds.
4. Tests only at sign-off: a leaf runs only a test it wrote or repairs; package tests run in the merge gate.
5. Pi or an external agent does the work and fixes; a Claude subagent verifies and commits in the lane (a relay type
   cannot commit, so the main session does); the main session lands ([Landing](#landing)).
6. In a contract the Opus owner runs this fan-out; otherwise main.

##### Owner layer (the contract)

Most tasks are led by an Opus owner with a fleet on Sonnet or Haiku executing, down to one function or a test repair.
The owner does a unit alone or sub-contracts it, and checks every leaf it dispatched. The fixed checklist (create the
lane, confirm the dispatch did not die, lint passes with no syntax errors, tick the plan, close the proxy) is for Go to
perform and record, never the model. The eyeball check that tests would pass is the owner's own judged verdict, and a
test step or test repair still needs executed tests.
Built (plan 2026-11-24-typed-pi-dispatch, phases 10-12): the `contract` key of `agents ask --brief` (`{"unit", "kind":
function|test-repair|file|task, "limits": {"tokens", "leaves"}, "role": owner|fleet, "parent"}`; `--dry-run` prints
its `terms:`). Go records the terms it observes (gate, tests, handback, report; lane, tick and landed when the run has
a lane or plan task, closed at `worktree commit` and `land`) and the budget; the scoped-proxy terms are not wired. A
fleet leaf stays terminal: it never dispatches, commits (bar the verifier's lane commit) or runs the suite.

#### Wave loop

The main thread plans waves, dispatches, decides and commits; file reads, diff review, gate runs and review fixes go to
leaves that hand back a short verdict, so its context stays small. Run it without prompting:

1. **Queue:** the [Dispatch table](#dispatch-table) holds the blocked tasks; dispatch one the moment its blocker
   frees, and again after every hand-back, until none remain or what is left needs the owner.
2. **Per hand-back:** a review leaf returns PASS/FAIL, ranked findings and the exact files to stage (it measures and
   reads the diff, not the manager) → a fix leaf for each finding (review loop until clear;
   the manager fixes what the leaf could not, R28) → the manager re-runs the scoped gate and commits named files
   (`rules/standards/worktree-git.md`) → dispatch what that unblocked. Push, install and the audit follow
   `CLAUDE.md` hard rule 5 (the audit runs after each worktree land), not per wave.
3. **Bugs:** any bug or issue a leaf or review reports gets a fix leaf at once, without asking,
   within the owner's named priority (R25).
4. **Open items:** pick the recommended option, apply it and tag `DECISION:`; ask only for frozen hook code,
   `settings.json`, secrets or an irreversible outward action.
5. **Briefs that do not stall:** say what is already verified (never "survey, then stop"), name a hand-back line cap,
   and own each source file's `_test.go` (a test that must sit in another package's test file to avoid an import cycle
   is allowed); a leaf stops only for a production file another agent owns.

#### Dispatch table

Write it before the first dispatch of a multi-part request (after its dated plan, R18), re-check it on every hand-back
and every new user message (merge the new asks into it; never serve requests in arrival order), and dispatch every
unblocked row in one message.

| Task | Owns | Blocked by | Agent |
| --- | --- | --- | --- |
| A: fix flag parse | `/r/internal/cli/args.go` | none | claude |
| B: fix flag help | `/r/internal/cli/args.go` | A (lane 2 of 2) | claude (lane of A) |
| C: add retry | `/r/internal/net/retry.go` | none | copilot (carried) |
| D: review A | none | A | codex (carried) |

- **Hot file:** when 2+ queued tasks own one file, first dispatch a split (extract by function into a new file in the
  same package, behavior unchanged) so they run in parallel; if not worth it, show that file's queue and length.
- **Lane:** `SendMessage` / `claude-code agents send <id> <task>` is for work in files that leaf already owns; show
  the lane's length, and past 2 queued items start a second leaf on a different file instead of appending a third.
- **Know what leaves hold:** ask, never assume the brief: every brief has the leaf list the files actually changed in
  its report; while it runs, `rtk git status --short`, the `worktree-claim` files (`rules/harness/hooks.md`) and
  `claude-code agents tree --files` show what is dirty and who holds it, `agents status` who is live.
- **Review per finished file or package** as each implementer hands back, not after all of them.
- **Gates lock the tree, not the dispatcher:** during a timed `make audit`, an install or a live check, keep
  dispatching reads and work outside the tree (scratch dirs, a linked worktree); queue only the commits.
- **Lanes and landing:** each stream's leaves work in their own lane (`HARNESS_WORKTREE=lanes` or an approved
  worktree) and each lane lands as its task completes ([Landing](#landing)).
- **Scouts and reviews** never wait for edit work.

#### Tests

The implementer writes the basic tests for its own feature (R15); other cases are proposals in its hand-back. Broken
existing tests are repaired by the implementer, a `claude` leaf or a carried `copilot` ask. The main thread owns the
live check. Loop: `rules/standards/testing-guidelines.md`. A leaf that fails one test 3 times reports and stops
(`rules/standards/testing-guidelines.md` → When a test fails).

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one carried `copilot` ask naming the TEST section |
| Failures across packages | one carried `copilot` ask per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | the implementer writes its basic tests; a repair leaf only for tests the change broke |
| A fixed bug | after your live before/after, the regression test is a proposal the owner approves |
| Suite must go green, bounded | `Workflow code-looper` — only when the user asks |

### Phase 3: integrate

#### Fan-in

Treat every report as a claim, verified by a review leaf (diff, deleted blocks via callers and `git log -S`, edits
outside the leaf's files, a new test that can fail); the manager reads its verdict, re-runs the scoped check and
rebuilds before trusting a binary ([Wave loop](#wave-loop)). Leaves hand back uncommitted unless the brief says to
commit; main verifies, then `worktree commit --task` ticks.

- Parallel leaves that built both sides of a seam (flag/argv builder ↔ consumer, JSON shape ↔ reader, hook key ↔
  brief writer): read both together; `rtk go build ./...` misses a broken consumer.
- Disagreeing reviewers: keep both findings with sources. Simplify once over the merged diff, then review.
- **Saved workflow output** is a claim too, with its own run rules:
  [saved-workflows.md](references/saved-workflows.md) → Fan-in and run rules.

#### Landing

When the change is authorized (an edit-only task stays uncommitted, `CLAUDE.md` hard rule 5), each fan-out lane lands
on the live checkout (uncommitted; you commit) the moment its hand-back verifies, conflicts resolved in the lane,
without waiting for siblings (`rules/standards/worktree-git.md` → Land on completion; verbs and refusals:
`skills/using-git-worktrees/SKILL.md` → The lane protocol). A diff that adds a restriction (block, deny, refusal,
narrower sandbox) waits for the owner's yes; the `lane-land` hook lands a clean `UNFIXED: none` lane itself, the rest
sit in the `land queue:` line.

1. Commit in the lane, named files only: `claude-code worktree commit --task <id> --plan <file> -m <msg> <files>` (the
   relay's `commit+tick:` line prints it; `--dry-run` previews the set).
2. Land: `claude-code worktree land --resolve-agent pi <lane>`, flags before the name, `--dry-run` to preview.
   `<lane>` is the worktree directory name (`agent-<id>`), never the branch (`worktree-agent-<id>`: "not found").
3. The land rebases the lane onto main, gates it (build and vet everywhere; tests of the changed packages and their
   direct importers, the whole suite when `go.mod`, `go.sum` or an unowned non-Go, non-Markdown file changed), applies
   the net diff uncommitted and removes the worktree (the branch stays); a detached post-land audit (`make audit`)
   reports on your next prompt.
4. Refusals: exit 3 when a landed path is dirty in the live tree (nothing changed; commit or move that edit, re-run)
   or the rebase conflicts and `--resolve-agent` leaves it unresolved (lane untouched; resolve in the lane, re-run, or
   `--resume`). `promote: target moved … since integration` means a peer committed meanwhile: re-run.
5. After you commit the landed diff, `claude-code worktree clean <lane>` deletes the kept branch once its diff is in
   `HEAD`; `--force` discards unmerged work.

### Phase 4: finish

Checks: `rules/standards/quality.md`; commits only when authorized: `rules/standards/worktree-git.md`.

#### Return to owner

A gap goes back to the same agent with `SendMessage` (the gap, evidence wanted, stop condition). Never poll or
`sleep` yourself: a long `ask` runs `--background`, then foreground `result -f <id>` (600000 ms, rerun only on a
timeout, at most 12 times; [agent-verbs.md](references/agent-verbs.md) → Procedure). Re-dispatch only when the owner is
gone; after a restart read `git status`; never relaunch a leaf the user stopped. Reports go to
`$(claude-code memory path reports YYYY-MM-DD-<name>.md)`.

#### Plan upkeep

Plans live in `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md`; a task ticks only through the main session's
`worktree commit --task` (or its commit of a landed lane) after it verifies a hand-back. A task really dropped is
struck, not ticked: `claude-code plan task <id> --reason superseded|duplicate|obsolete|out_of_scope --evidence
decision:<id>|commit:<sha>|note:<path>` (no other reason or cite is accepted; quote an id with a space, `"Phase 2.1"`).

## Shared tree

The main shell stays at the repo root (use absolute paths, never a bare `cd`). Every brief says: never `git stash` /
`checkout` / `reset` / `restore`; gate on a scoped target (a repo-wide red is
a sibling's half-finished edit); run `claude-code agents diff --check --path <files>`, fix your own files
(`agents fix --path <files>`), never `make format`, `audit` or `install`; edit files with Edit or Write, never a
`python3 -` script (`safety-scan` asks on interpreter writes); check writes with `rtk git diff --stat`; stage named
files only.

## Unit files (worktrees only)

Inside a linked worktree, split work into units, not files, so leaves that touch one file still run in parallel.
Everything converges in the worktree before it merges onto the source branch; the live checkout never sees a unit file.
It applies only when work runs in a linked worktree: never under vault `HARNESS_WORKTREE=off`, always under `lanes`,
otherwise when the owner says yes to a worktree (`rules/standards/worktree-git.md` → Worktrees).

- **New code goes in a unit file.** A leaf adding functions or tests writes them to its own file beside the target:
  `<file>_<task>.go`, `<file>_<task>_test.go` (Go compiles a package's files together, so it builds and tests at once;
  elsewhere, new test files plus the fold step). The dispatch table lists units, each owning its unit file.
- **Edits to existing code** still own the real file, one leaf per file, as today.
- **Fold before merge.** One fold leaf moves each unit into its home file (functions beside their callers, tests into
  the `_test.go` matching their source), deletes the unit files, and resolves two units changing the same code as a
  branch conflict, keeping both intents; give it every brief, not only the diffs. Then one gate over the folded tree
  (build, vet, touched-package tests, `agents diff --check`), and only then `worktree commit` and `worktree land`.
- **Never commit a unit file.** It breaks the one-test-file-per-source rule (`rules/harness/go.md`); the gate catches
  one left behind.
- A test leaf proves teeth on a scratch copy (`go test -overlay`), never by editing source an editor leaf owns.

## Rules

- **Visible dispatch (owner preference from 2026-11-12; lifted for long jobs on 2026-11-20):** a dispatch goes through
  the Agent tool with a harness type (`claude`, Pi first by default; external work agents as their own type or
  carried, Pi as a plain ask). A job expected to outlast a Bash call (~10 minutes) may instead be started by the main
  session itself with `claude-code agents ask --background`, then `agents wait <id>... --timeout 9m` (repeat to
  cover 15m) and `agents result <id>`: no leaf sits waiting on a shell. A long ask started from a leaf carries
  `--background` too: the stop gate
  holds the leaf until the job ends (`dispatch-stop`, `dispatch-handback`), whereas a shell-backgrounded ask dies when
  the leaf stops. Cap: `rules/harness/hooks.md` → (dispatch cap); queue the rest.
- Decide the owner before gathering evidence, never redoing its searches. Simplify runs once per diff, by the manager
  (`rules/standards/quality.md` → Verification Order step 5); detectors (`claude-code review *`) once per pipeline.
- Preserve explicit agent/model choices unless the owner authorizes a change; models go on
  `ask --overlay '{"model":"<m>"}'`.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails | Retry once on a `claude` leaf with its failure output; a second failure: the main session finishes it where it can and says so, else lands the rest and names the gap |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong |
| Timeout | Keep partial results, cancel the rest |
| A leaf touched a file it didn't own | Report it; revert only a hunk its own diff proves it made (a file dirty at dispatch is the owner's or a peer's); re-dispatch with `Owns:` sharpened |
| Block on a leaf | `safety-scan` blocks only a `settings.json` deny match or an AI-attribution footer, for every agent; main does the blocked step itself or the owner edits the deny rule |
| External agent unavailable (credential or route missing, e.g. no M2M secret) | Report BLOCKED with the fix command; never silently do it in-context |
| Quota pressure | Dispatches keep their model; route the work to another agent yourself ([cost-routing.md](references/cost-routing.md)) |

## Test scenarios

- **Normal**: two `claude` leaves on disjoint packages in one message; fan-in reads each diff and the seam, simplify
  runs once, one commit names the files and task ([Landing](#landing)).
- **Error**: a leaf fails `Verify:` twice; the others land and the report names the gap (see Error handling).
