# Lane Orchestrator

Many leaf-sized tasks can be grouped into lanes of disjoint files. The main session owns the lane checklists,
dispatch, fan-in and commits; a lane is a scheduling group, not another manager subagent.
Dispatch syntax and relay behavior: `../SKILL.md` → Leaf types. Keep leaves terminal.

## Lane plan

Keep `lanes/<lane>.md` under the plans home beside the plan (`rules/harness/documentation.md` → Placement); a
convention, no gate reads it. Each checklist line names one verifiable outcome, owned absolute paths, the selected leaf
type and its dependencies. Split tasks before they need a nested orchestration layer. One writer per file; a later
wave owns overlapping work.

## Dispatch and verify

- The manager sends ready, disjoint leaves together, with a complete brief per `prompts.md`; each owns one checklist
  line. New tests wait for the manager's live check; repairs of existing tests may run beside implementation.
- Fan-in: `../SKILL.md` → Fan-in.
- Collection, landing and commit authority: `rules/harness/workflow-tree.md` → Worktrees.
  Record checked results without a hash when commits are held; add the resulting hash only after an authorized commit.

Explicitly requested batch scripts use `claude-code agents ask --batch` per its help. They do not bypass ownership,
permission or per-account concurrency limits, and are not a hidden fallback for unavailable Agent-tool dispatch.

## Owner layer (the contract)

The `contract` key of `agents ask --brief` describes a unit, its budget and owner/fleet role:
`unit`, `kind: function|test-repair|file|task`, `limits: {tokens, leaves}`, `role: owner|fleet`, `parent`.
`--dry-run` prints its terms. Go records observed gates, tests, hand-back and reports, plus lane and plan events;
scoped-proxy terms are not wired. The owner judges the evidence; an eyeball check does not replace executed checks.
A fleet leaf stays terminal; use the validation and commit rules in `../SKILL.md` → Dispatch workflow.

## Wave loop

Main plans waves, dispatches, decides and integrates:

1. Dispatch each row when its dependencies and owned files are free; refill after every hand-back.
2. Collect completed work through `rules/harness/workflow-tree.md` → Worktrees; fan-in does not start a lane review.
3. Fix reported defects within the owner's priority; main handles what a leaf could not fix (R25, R28).
4. Act on routine decisions and record `DECISION:`; approval boundaries remain in `rules/standards/principles.md`.
5. Give briefs verified context, a line cap and the matching test files; another package's test file is allowed
   when needed to avoid an import cycle. A leaf stops for production files another agent owns.

## Dispatch table

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
- **Know what leaves hold:** every brief has the leaf list the files actually changed in its report; while it runs,
  `rtk git status --short`, the `worktree-claim` files (`rules/harness/hooks.md`) and `claude-code agents tree --files`
  show what is dirty and who holds it, `agents status` who is live.
- **Gates lock the tree, not the dispatcher:** during a timed `make audit`, an install or a live check, keep
  dispatching reads and work outside the tree (scratch dirs, a linked worktree); queue only the commits.
- **Scouts** can run beside edit work; final review follows `../SKILL.md` → Dispatch workflow.

## Unit files (worktrees only)

Inside a linked worktree, split work into units, not files, so leaves that touch one file still run in parallel.
Everything converges in the worktree before it merges onto the source branch; the live checkout never sees a unit file.

- **New code goes in a unit file.** A leaf adding functions or tests writes them to its own file beside the target:
  `<file>_<task>.go`, `<file>_<task>_test.go`. The dispatch table lists units, each owning its unit file.
- **Edits to existing code** still own the real file, one leaf per file.
- **Fold before merge.** One fold leaf moves each unit into its home file, deletes the unit files, and resolves two
  units changing the same code as a branch conflict, keeping both intents; give it every brief, not only the diffs.
- **Never commit a unit file.** It breaks the one-test-file-per-source rule (`rules/harness/go.md`); the gate catches
  one left behind.
- A test leaf proves teeth on a scratch copy (`go test -overlay`), never by editing source an editor leaf owns.

## Test scenarios

- **Normal**: dispatch disjoint leaves, collect each hand-back, then follow the collector sign-off.
- **Error**: main finishes a failed leaf's partial work before sign-off; report an unresolved blocker.
