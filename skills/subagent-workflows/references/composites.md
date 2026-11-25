# Composite Workflow Templates

Fill-in templates for the harness's composites. Shapes: [patterns.md](patterns.md); args and exits:
[saved-workflows.md](saved-workflows.md). Run one only when the owner asks; with no specific fit, run (A).

Cost rule of thumb: about 78k tokens per agent (26 agents / 2.0M, 60 / 4.8M, 114 / 8.8M); the code-workers estimate is
`6 x packages + 3 x fixed packages` (the script logs it at launch), plus 2 (size, record) and polish (+2-3, and +3 per
package with review findings). The dispatch cap claims one slot per `agent(` call site in the script text (code-workers
5, code-engineer 11, code-evaluator 4, code-analyzer 2, code-looper 1), clamped to the per-account cap (24) and the
machine total, not the real agent count.

## A. code-engineer: size, route, stream, record

- **Use when**: any code job when no specific workflow fits; TRIVIAL to HIGH (EPIC stops at a plan).
- **Script**: `~/.claude/workflows/code-engineer.js` calling
  `~/.claude/workflows/code-workers.js`.

| Stage    | Agent / model, effort       | Input                              | Output                                                                                                                       | Gate / check                                                                              | Recorded         |
| -------- | --------------------------- | ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- | ---------------- |
| Size     | sizer, `haiku` low          | task, `git status`, `go list`      | `{kind, packages, paths, focus, reason}`; the script computes the tier                                                       | kind enum fix\|feature\|refactor\|audit\|consolidate\|architecture                        | decision line    |
| Route    | none (code)                 | tier                               | `code-workers:paths` · `code-workers:packages` · `plan` (prefixed `code-analyzer→` or `code-evaluator→` when they run first) | EPIC returns early                                                                        | route line       |
| Run      | code-workers (composite B)  | paths or packages                  | code-workers result                                                                                                          | exit `green\|red\|review`                                                                 | child decisions  |
| Dedupe   | deduper, `sonnet` medium    | combined diff of 2+ fixed packages | applied, flagged, Δ lines                                                                                                    | only cross-package duplicated helpers; `polish:false` skips                               | decision         |
| Simplify | simplifier, `sonnet` medium | merged diff of all fixed files     | simplify result                                                                                                              | simplify gate                                                                             | decision         |
| Review   | reviewer, `sonnet` high     | that merged diff                   | findings                                                                                                                     | any finding (lows too) earns one Fix-all, test and gate per package (no second child run) | decision         |
| Record   | recorder, `haiku` low       | report, decisions                  | paths written                                                                                                                | two files exist                                                                           | the memory files |

- **Args**: [saved-workflows.md](saved-workflows.md) → `code-engineer` row. Exits `green|red|review|plan|error`, or a
  child's `blocked|stalled|incomplete|capped`.
- **Failure modes and guards**: sizer returns nothing, so pass `tier`; owner messages reach running agents (open);
  classifier outage blocked one fixer in R11 (reported red, re-dispatched by hand, `07f86477`).
- **Resume and results**: relaunch with the Workflow tool's `resumeFromRunId` and the same `scriptPath`; unchanged
  `agent()` calls replay from cache (R7's fix round re-ran only 12 new agents; R8's resumed the same way). Results land
  in `$(claude-code memory path reports DATE-<label>.md)` and `DATE-decisions.md`.

## B. code-workers: find, rederive, fix, test repair, gate, simplify, review, fix-all

- **Use when**: audit and repair named packages or files; `fix:false` for review only (R7, R8 shape).

| Stage                  | Agent / model, effort        | Input                                                                                                 | Output                                                                            | Gate / check                                                                                     | Recorded                          |
| ---------------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ | --------------------------------- |
| Find x2 lenses         | `sonnet` medium              | package                                                                                               | findings (file, line, symbol, severity)                                           | merge by file +-10 or symbol                                                                     | merged count                      |
| Rederive               | `sonnet` medium, blind       | file:line + symptom                                                                                   | reproduced, scenario, severity                                                    | reproduced=false drops; null = unverified                                                        | dropped, unverified               |
| Fix                    | `sonnet` medium              | confirmed >= `minFix`                                                                                 | `{status, files, callers}`                                                        | status `done` or red                                                                             | fixed count                       |
| Test                   | `steps.test` `sonnet` medium | the diff                                                                                              | repairs the tests the fix broke, writes no new test (proposed in `proposedTests`) | the tests pass                                                                                   | test report                       |
| Gate                   | `haiku` low                  | package                                                                                               | `{exit, residual, ...}` from `stageLoop`                                          | `claude-code worktree gate --scope <pkg>` (build ./..., vet, test of the package) or `args.gate` | green or red                      |
| Dedupe / Simplify      | `sonnet` medium              | once over the merged diff of the packages that went green after the stream (Dedupe needs 2+ packages) | applied, flagged, Δ lines                                                         | simplify gate; reverts its own regressing edit; skipped if `polish:false` or budget < 150k       | "simplify: N applied"             |
| Review                 | `sonnet` high                | that merged diff                                                                                      | findings (file, line, symbol, symptom, severity)                                  | correctness + contract lenses                                                                    | "review: N findings (sev counts)" |
| Fix-all, Test, re-Gate | `steps.fix`, `test`, `gate`  | every review finding, lows included                                                                   | fixed count; repaired tests; `{green}`                                            | each package with review findings gets one cycle; red re-gate or unfinished fix-all = red        | "fix-all: N fixed"                |

- **Foreign re-route**: a finding citing a file outside its finder's package is fixed after the stream by the owning
  directory (`d51420fb`); a live re-route run is still owed.
- **Args**: [saved-workflows.md](saved-workflows.md) → `code-workers` row; `slice` (first N packages) for a first run.
- **Failure modes**: scoped gate misses cross-package breakage (`16d36662`): full suite before push; fixer blocked
  but gate green is red (`07f86477`); an out-of-scope hunk in a fix is reverted by the supervisor.
- **Results**: returns `{exit, route, packages, merged, confirmed, dropped, unverified, fixed [{pkg, findings,
  attempted, status, landed, impl, test, gate, files}], red, residual, polish, files, foreign, seams, lines,
  proposedTests, over, gates, decisions, report, coworkers}`; direct runs record nothing, so go through (A) when the
  run should be filed.

## C. MEDIUM feature: intel, implementer, code-workers harden

- **Use when**: a task of 2 to 8 files in at most 3 dirs whose `kind` is feature or refactor (`kind` arg, else the
  sizer's; with neither, the leading task verb: `build|add|implement|create|introduce|support|write|finish|refactor|
  move|migrate|replace|rewire|fold|extract|retire|remove|delete|rename|phase`); this is code-engineer's MEDIUM route.

| Stage       | Agent / model, effort     | Input              | Output              | Gate / check                   | Recorded        |
| ----------- | ------------------------- | ------------------ | ------------------- | ------------------------------ | --------------- |
| Intel       | `sonnet` medium           | task, sized paths  | `{paths, focus}`    | empty keeps sized paths        | decision        |
| Implementer | `sonnet` medium           | files, task, focus | `{status, summary}` | gofmt, go vet; writes no tests | status line     |
| Harden      | code-workers on the files | paths              | composite B result  | test repair, gate              | child decisions |

- **Call**: `Workflow({scriptPath: ".../code-engineer.js", args: {cwd, task: "add X", tier: "MEDIUM"}})`.
- **Failure modes**: code-workers only fixes bugs, so a build is the implementer's job.
- **Resume**: re-run with `paths` set to the files the implementer touched and `kind: 'fix'`, which skips it.

## D. EPIC: plan draft, owner approval, rounds over package groups

- **Use when**: multi-session or architectural work, or an audit of the whole tree. R7 to R11 ran this way.

- **Stages**: Draft (`Plan` or the code-engineer EPIC route, `docs/templates/plan.md`; **owner approval**, filed under
  `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md`) → Group (main session: package groups G1..Gn, each fitting one run of
  6 to 15 packages) → Round (composite A or B per group; full suite before push; report + decisions) → Fix leftovers
  (a `claude` leaf on red or foreign findings; per-package gate).
- **Policy**: fix every finding, LOW included, in the round, no per-round ask; only owner-judgment items go to a report.
- **Failure modes**: leaving mediums unfixed before the next round left earlier gaps.
- **Resume**: continue at the first unticked plan line or the next group; one report per round
  (`reports/DATE-r<n>-g<m>.md`).

## E. Main-session supervisor loop around a workflow

`launch (A|B) → wait (never poll) → read result → verify diffs → scoped gate → commit → land → release checks`

- Verify the diff (`rtk git diff --stat`, only the run's packages, out-of-scope hunks reverted; `../SKILL.md` →
  Fan-in); re-dispatch by hand a red package or a `fixed` entry whose status is not done.
- Commit named files with `claude-code worktree commit --task <id> -m <msg> <files>` (no AI footer) and land a lane per
  `../SKILL.md` → Landing. The full suite, `make install` from a clean tree and push follow `CLAUDE.md` hard rule 5 at
  the authorized release boundary.
- Log decisions and reverts in `reports/DATE-decisions.md` (`claude-code memory report`).

## F. code-analyzer partition: scan, plan, waves

Use when a fix touches several packages and merge conflicts cost more than the serial time. After Refute, one
`depscan` agent per package returns `{files, exported, importers, hotspots}`; a script (no agent) builds a conflict
graph (shared file, or an exported change imported by the other package), colours it greedily into waves of at most
`cap` (default 8, never over the dispatch cap, `rules/harness/hooks.md` → (dispatch cap)), and pulls hotspot files
(`HOTSPOTS` plus `args.hotspots`) into one final single-owner task. Waves run in sequence, tasks of a wave in
parallel; each fixer owns only its listed files.
`fix:false, partition:true` returns `partition {waves, hotspotTask, edges}` and a `## Tasks` plan block, no fixes.

Waves, not a worktree per writer: no worktree without the owner's yes (`HARNESS_WORKTREE=lanes` asks nothing; lanes are
per task, `rules/standards/worktree-git.md` → Worktrees), and waves give the same disjointness with no per-writer setup
or merge.

## New composite: template

```text
Name:        <workflow-name>            (file workflows/<workflow-name>.js, meta.name equal)
Use when:    <tier / job size / trigger>
Pattern:     <pipeline | fan-out | expert pool | producer-reviewer | hierarchical>   (patterns.md)
Stages:      | stage | agent type + model/effort | input | output schema | gate | recorded |
Args:        cwd, <inputs>; optional fix, minFix, focus, slice, steps {<stage>: {model, effort}}
Cost:        <agents per unit> x <units> x ~78k tokens; slice first
Failure:     <mode> -> <guard / fix commit>
Resume:      <resumeFromRunId | rerun args>   Results: <reports path>
Returns:     {exit, route, packages, merged, confirmed, dropped, unverified, fixed, red, decisions, report}
```

### Checklist: adding it as a saved workflow

- `meta` is a pure literal with `name`, `description`, `whenToUse` (USE / CALL / OPTIONAL / RETURNS) and `phases`; plain
  JavaScript, `ERR(...)` for bad args, no shell state outside agents; `agent()` pins and the table row:
  [saved-workflows.md](saved-workflows.md) → Authoring.
- Mark the `stepOf` block (`// <block:stepOf>` to `// </block:stepOf>`) and run `claude-code workflow sync`: it writes
  the embedded blocks (`internal/assets/files/workflows/blocks/`, the source) into every marked script.
- Return the shared result shape above; log the agent estimate at launch and take a `slice` arg for the first run.
- Run once on a small slice, then update the table and `whenToUse`.
