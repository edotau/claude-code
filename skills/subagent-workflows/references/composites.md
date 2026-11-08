# Composite Workflow Templates

The composites this harness operates from, as fill-in templates. Shapes and why: [patterns.md](patterns.md); args and
exits: [saved-workflows.md](saved-workflows.md). Run one only when the owner asks; with no specific fit, run (A).
Evidence: `memory/claude-code/reports/2026-11-07-workflow-scorecard.md`.

Cost rule of thumb (R9-R11): about 78k tokens per agent (26 / 2.0M, 60 / 4.8M, 114 / 8.8M). code-relay estimate is
`6 x packages + 3 x fixed packages` (the script logs it at launch) plus 2 (size, record). Dispatch cap:
24 per account, one slot per `agent()` site, so a 15-package run is claimed at the cap, not its real count.

## A. code-engineer: size, route, stream, record

- **Use when**: any code job when no specific workflow fits; TRIVIAL to HIGH (EPIC stops at a plan).
- **Script**: `/home/edotau/.claude/workflows/code-engineer.js` calling `/home/edotau/.claude/workflows/code-relay.js`.

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Size | sizer, `haiku` low | task, `git status`, `go list` | `{tier, packages, paths, focus}` | tier enum | decision line |
| Route | none (code) | tier | `code-relay:paths` · `code-relay:packages` · `plan` | EPIC returns early | route line |
| Run | code-relay (composite B) | paths or packages | code-relay result | exit `green\|red\|review` | child decisions |
| Dedupe | deduper, `sonnet` medium | combined diff of 2+ fixed packages | applied, flagged, Δ lines | only cross-package duplicated helpers; `polish:false` skips | decision |
| Simplify | simplifier, `sonnet` medium | merged diff of all fixed files | simplify result | simplify gate | decision |
| Review | reviewer, `sonnet` high, read-only | that merged diff | findings | any finding (lows too) earns one more code-relay, capped at one loop | decision |
| Record | recorder, `haiku` low | report, decisions | paths written | two files exist | the memory files |

- **Args**: `cwd` and one of `task`, `packages`, `paths`, `range` (git range; changed files become paths); optional
  `tier`, `fix` (true), `minFix` (low), `focus`,
  `polish` (true; false skips code-relay's per-package chain and the final pass), `label`,
  `record:false`, `steps {size, intel, impl, plan, dedupe, simplify, review, record, child}`.
  Exits `green|red|review|plan|error`.
- **Cost**: HIGH over N packages is code-relay's estimate plus 2; R10 6 packages 60 agents, R11 15 packages 114.
- **Failure modes and guards**: sizer returns nothing, so pass `tier`; untested routes (LOW/MEDIUM/impl/EPIC) went
  live only in `2026-11-07-route-test-low.md`; owner messages reach running agents (R8, open); classifier outage
  blocked one fixer in R11 (reported red, re-dispatched by hand, `07f86477`).
- **Resume and results**: relaunch with the Workflow tool's `resumeFromRunId` and the same `scriptPath`; unchanged
  `agent()` calls replay from cache (R7's fix round re-ran only 12 new agents; R8's resumed the same way). Results land
  in `$(claude-code memory path)/reports/DATE-<label>.md` and `DATE-decisions.md`.

## B. code-relay: find, rederive, fix, guard test, gate, simplify, review, fix-all

- **Use when**: audit and repair named packages or files; `fix:false` for review only (R7, R8 shape).

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Find x2 lenses | `sonnet` medium, read-only | package | findings (file, line, symbol, severity) | merge by file +-10 or symbol | merged count |
| Rederive | `sonnet` medium, blind | file:line + symptom | reproduced, scenario, severity | reproduced=false drops; null = unverified | dropped, unverified |
| Fix | `sonnet` medium | confirmed >= `minFix` | `{status, files, callers}` | status `done` or red | fixed count |
| Guard test | `sonnet` medium | the diff | one test per fix | `claude-code test mutate` exit 0 | test report |
| Gate | `haiku` low | package | `{green, output}` | build, vet, test; plus `internal/schemas` for a writer | green or red |
| Simplify | `sonnet` medium | the package's changed files | applied, flagged, Δ lines | simplify gate; reverts its own regressing edit; skipped if `polish:false`, budget < 150k, or fix not done/green | "simplify: N applied" |
| Review | `sonnet` high, read-only | the package diff | findings (file, line, symbol, symptom, severity) | correctness + contract lenses | "review: N findings (sev counts)" |
| Fix-all, Test, re-Gate | `steps.fix`, `test`, `gate` | every review finding, lows included | fixed count; guard tests; `{green}` | one cycle per package; red re-gate or unfinished fix-all = red | "fix-all: N fixed" |

- **Foreign re-route**: a finding citing a file outside its finder's package is fixed after the stream by the owning
  directory (`d51420fb`); a live re-route run is still owed (scorecard rank 2).
- **Args**: `cwd`, `packages` or `paths`, `slice` (first N packages, a small first run), `fix`, `minFix`, `focus`,
  `polish` (true), `steps {find, rederive, fix, test, gate, simplify, review}`.
- **Cost**: `6 x P + 3 x fixed` (+4 per fixed package with polish: simplify, review,
  and when findings exist fix-all, test, re-gate); yield 7.0 to 7.5 confirmed findings per 1M tokens.
- **Failure modes**: scoped gate misses cross-package breakage (`16d36662`): full suite before push; fixer blocked
  but gate green is red (`07f86477`); an out-of-scope hunk in a fix is reverted by the supervisor (decision #20).
- **Results**: returns `{exit, confirmed, dropped, unverified, fixed (each with simplify, review, fixall), red,
  decisions, report}`; direct runs record nothing, so go through (A) when the run should be filed.

## C. MEDIUM feature: intel, implementer, code-relay harden

- **Use when**: a 2 to 5 file task with a feature verb (`build|add|implement|create|introduce|support|write`, not
  `audit|review`); this is code-engineer's MEDIUM route.

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Intel | `sonnet` medium, read-only | task, sized paths | `{paths, focus}` | empty keeps sized paths | decision |
| Implementer | `sonnet` medium | files, task, focus | `{status, summary}` | gofmt, go vet; writes no tests | status line |
| Harden | code-relay on the files | paths | composite B result | guard test, gate | child decisions |

- **Call**: `Workflow({scriptPath: ".../code-engineer.js", args: {cwd, task: "add X", tier: "MEDIUM"}})`.
- **Cost**: 2 + code-relay on 1 to 3 packages, about 8 to 20 agents (about 0.6M to 1.6M tokens, extrapolated).
- **Failure modes**: code-relay only fixes bugs, so a build is the implementer's job; the old `code-engineer`
    (one fixed chain) was rewritten as the size-routed script, renamed from `orchestrate`
    (A/B: `memory/claude-code/reports/2026-11-07-workflow-trial-decisions.md`).
- **Resume**: re-run with `paths` set to the files the implementer touched and no feature verb, which skips it.

## D. EPIC: plan draft, owner approval, rounds over package groups

- **Use when**: multi-session or architectural work, or an audit of the whole tree. R7 to R11 ran this way.

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Draft | `Plan` or code-engineer EPIC route | task, `docs/templates/plan.md` | plan markdown | **owner approval** | `plans/DATE-<slug>.md` |
| Group | main session | plan | package groups G1..Gn | group fits one run (6 to 15 packages) | group list |
| Round | composite A or B per group | group packages | confirmed, fixed, red | full suite before push | report + decisions |
| Fix leftovers | `claude` leaf | red or foreign findings | commit | per-package gate | decision row |

- **Policy** (#17): fix CRITICAL/HIGH/MEDIUM in the round, LOW to `workflow-trial-techdebt.md`, no per-round ask.
- **Cost**: R7 37 agents, R8 74, R9 26, R10 60, R11 114 (5 rounds, about 21M tokens including relay2).
- **Failure modes**: fixing mediums before the next round left earlier gaps (#12, #15); update the scorecard after
  each round; a plan not dated from first write (`memory/claude-code/feedback-plans-dated-template-first.md`).
- **Resume**: continue at the first unticked plan line or the next group; reports are per round
  (`reports/2026-11-07-r<n>-g<m>.md`).

## E. Main-session supervisor loop around a workflow

```text
launch(A|B) -> wait (never poll) -> read result -> verify diffs -> scoped gate -> full suite -> commit named files -> push
```

| Step | Check | Source |
| --- | --- | --- |
| Verify diff | `git diff --stat`; only the run's packages; revert out-of-scope hunks | decision #20 |
| Re-run red gates | a red package or `fixed` with status not done is re-dispatched by hand | `07f86477` |
| Full suite | `go build ./... && go vet ./... && go test ./...` before every push | #27 |
| Commit | `git commit -- <paths>`, named files, no AI footer; merge, `make install` from a clean tree, push | CLAUDE.md rules 3, 5 |
| Log | decisions and reverts to `reports/DATE-decisions.md`; scorecard row updated | `workflow-scorecard.md` |

Never `git stash|checkout|reset|restore|clean` in the live tree; peers share it (CLAUDE.md rule 4).

## F. code-analyzer partition: scan, plan, waves

Use when a fix touches several packages and merge conflicts cost more than the serial time. After Refute, one
`depscan` agent per package returns `{files, exported, importers, hotspots}`; a script (no agent) builds a conflict
graph (shared file, or an exported change imported by the other package), colours it greedily into waves of at most
`cap` (default 8, never over the 24 dispatch cap), and pulls hotspot files (`HOTSPOTS` plus `args.hotspots`) into one
final single-owner task. Waves run in sequence, tasks of a wave in parallel; each fixer owns only its listed files.
`fix:false, partition:true` returns `partition {waves, hotspotTask, edges}` and a `## Tasks` plan block, no edits.

Why disjoint dirs and waves, not `isolation: 'worktree'`: the house rule is no worktree without the owner's yes
(`rules/standards/worktree-git.md`), a worktree costs setup and a merge per writer, and package-disjoint writers
or conflict-free waves in the shared tree give the same disjointness.

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

- Script in `/home/edotau/.claude/workflows/<name>.js`; `meta` is a pure literal with `name`, `description`,
  `whenToUse` (USE / CALL / OPTIONAL / RETURNS) and `phases`.
- A row in the table of [saved-workflows.md](saved-workflows.md); `claude-code workflow lint` fails on a script
  missing from it or a row naming no script.
- Copy `stepOf` verbatim (markers `── stepOf: copied verbatim` to `── end stepOf ──`) from
  `/home/edotau/.claude/workflows/code-relay.js`; every stage reads its pin through it.
- Every `agent()` call pins `model:` and `effort:`; claude only, no `agent`/`profile` fields.
- Return the shared result shape above; `ERR(...)` for bad args; no shell state outside agents; plain JavaScript.
- A log line with the agent estimate at launch, and a `slice` arg for the first run.
- Run once on a small slice, add the scorecard row, then update the table and `whenToUse`.
