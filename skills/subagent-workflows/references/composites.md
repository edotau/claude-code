# Composite Workflow Templates

Fill-in templates for the harness's composites. Shapes: [patterns.md](patterns.md); args and exits:
[saved-workflows.md](saved-workflows.md). Run one only when the owner asks; with no specific fit, run (A).

Cost rule of thumb (R9-R11): about 78k tokens per agent (26 / 2.0M, 60 / 4.8M, 114 / 8.8M). code-workers estimate is
`6 x packages + 3 x fixed packages` (the script logs it at launch) plus 2 (size, record). Dispatch cap:
24 per account; the hook claims one slot per `agent(` call site in the script text (code-workers 5, code-engineer 11,
code-evaluator 4, code-analyzer 2, code-looper 1), clamped to the 24 per-account cap and the machine total, so a run
is claimed at its script's call-site count, not its real agent count.

## A. code-engineer: size, route, stream, record

- **Use when**: any code job when no specific workflow fits; TRIVIAL to HIGH (EPIC stops at a plan).
- **Script**: `~/.claude/workflows/code-engineer.js` calling
  `~/.claude/workflows/code-workers.js`.

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Size | sizer, `haiku` low | task, `git status`, `go list` | `{kind, packages, paths, focus, reason}`; the script computes the tier | kind enum fix\|feature\|refactor\|audit\|consolidate\|architecture | decision line |
| Route | none (code) | tier | `code-workers:paths` · `code-workers:packages` · `plan` (prefixed `code-analyzer→` or `code-evaluator→` when they run first) | EPIC returns early | route line |
| Run | code-workers (composite B) | paths or packages | code-workers result | exit `green\|red\|review` | child decisions |
| Dedupe | deduper, `sonnet` medium | combined diff of 2+ fixed packages | applied, flagged, Δ lines | only cross-package duplicated helpers; `polish:false` skips | decision |
| Simplify | simplifier, `sonnet` medium | merged diff of all fixed files | simplify result | simplify gate | decision |
| Review | reviewer, `sonnet` high, read-only | that merged diff | findings | any finding (lows too) earns one Fix-all, test and gate per package (no second child run) | decision |
| Record | recorder, `haiku` low | report, decisions | paths written | two files exist | the memory files |

- **Args**: [saved-workflows.md](saved-workflows.md) → `code-engineer` row. Exits `green|red|review|plan|error`, or a
  child's `blocked|stalled|incomplete|capped`.
- **Cost**: HIGH over N packages is code-workers's estimate plus 2; R10 6 packages 60 agents, R11 15 packages 114.
- **Failure modes and guards**: sizer returns nothing, so pass `tier`; owner messages reach running agents (open);
  classifier outage blocked one fixer in R11 (reported red, re-dispatched by hand, `07f86477`).
- **Resume and results**: relaunch with the Workflow tool's `resumeFromRunId` and the same `scriptPath`; unchanged
  `agent()` calls replay from cache (R7's fix round re-ran only 12 new agents; R8's resumed the same way). Results land
  in `$(claude-code memory path reports DATE-<label>.md)` and `DATE-decisions.md`.

## B. code-workers: find, rederive, fix, test repair, gate, simplify, review, fix-all

- **Use when**: audit and repair named packages or files; `fix:false` for review only (R7, R8 shape).

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Find x2 lenses | `sonnet` medium, read-only | package | findings (file, line, symbol, severity) | merge by file +-10 or symbol | merged count |
| Rederive | `sonnet` medium, blind | file:line + symptom | reproduced, scenario, severity | reproduced=false drops; null = unverified | dropped, unverified |
| Fix | `sonnet` medium | confirmed >= `minFix` | `{status, files, callers}` | status `done` or red | fixed count |
| Test | `steps.test` `sonnet` medium | the diff | repairs the tests the fix broke, writes no new test (proposed in `proposedTests`) | the tests pass | test report |
| Gate | `haiku` low | package | `{exit, residual, ...}` from `stageLoop` | `claude-code worktree gate --scope <pkg>` (build ./..., vet, test of the package) or `args.gate` | green or red |
| Dedupe / Simplify | `sonnet` medium | once over the merged diff of the packages that went green after the stream (Dedupe needs 2+ packages) | applied, flagged, Δ lines | simplify gate; reverts its own regressing edit; skipped if `polish:false` or budget < 150k | "simplify: N applied" |
| Review | `sonnet` high, read-only | that merged diff | findings (file, line, symbol, symptom, severity) | correctness + contract lenses | "review: N findings (sev counts)" |
| Fix-all, Test, re-Gate | `steps.fix`, `test`, `gate` | every review finding, lows included | fixed count; repaired tests; `{green}` | each package with review findings gets one cycle; red re-gate or unfinished fix-all = red | "fix-all: N fixed" |

- **Foreign re-route**: a finding citing a file outside its finder's package is fixed after the stream by the owning
  directory (`d51420fb`); a live re-route run is still owed.
- **Args**: [saved-workflows.md](saved-workflows.md) → `code-workers` row; `slice` (first N packages) for a first run.
- **Cost**: `6 x P + 3 x fixed`, plus polish once over the merged diff: +2-3 agents
  and +3 per package with review findings; yield 7.0 to 7.5 confirmed findings per 1M tokens.
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

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Intel | `sonnet` medium, read-only | task, sized paths | `{paths, focus}` | empty keeps sized paths | decision |
| Implementer | `sonnet` medium | files, task, focus | `{status, summary}` | gofmt, go vet; writes no tests | status line |
| Harden | code-workers on the files | paths | composite B result | test repair, gate | child decisions |

- **Call**: `Workflow({scriptPath: ".../code-engineer.js", args: {cwd, task: "add X", tier: "MEDIUM"}})`.
- **Cost**: 2 + code-workers on 1 to 3 packages, about 8 to 20 agents (about 0.6M to 1.6M tokens, extrapolated).
- **Failure modes**: code-workers only fixes bugs, so a build is the implementer's job.
- **Resume**: re-run with `paths` set to the files the implementer touched and `kind: 'fix'`, which skips it.

## D. EPIC: plan draft, owner approval, rounds over package groups

- **Use when**: multi-session or architectural work, or an audit of the whole tree. R7 to R11 ran this way.

| Stage | Agent / model, effort | Input | Output | Gate / check | Recorded |
| --- | --- | --- | --- | --- | --- |
| Draft | `Plan` or code-engineer EPIC route | task, `docs/templates/plan.md` | plan markdown | **owner approval** | `plans/DATE-<slug>.md` |
| Group | main session | plan | package groups G1..Gn | group fits one run (6 to 15 packages) | group list |
| Round | composite A or B per group | group packages | confirmed, fixed, red | full suite before push | report + decisions |
| Fix leftovers | `claude` leaf | red or foreign findings | verified diff and scoped gate results | per-package gate | decision row |

- **Policy**: fix every finding, LOW included, in the round, no per-round ask; only owner-judgment items go to a report.
- **Cost**: R7 37 agents, R8 74, R9 26, R10 60, R11 114 (5 rounds, about 21M tokens).
- **Failure modes**: leaving mediums unfixed before the next round left earlier gaps.
- **Resume**: continue at the first unticked plan line or the next group; one report per round
  (`reports/DATE-r<n>-g<m>.md`).

## E. Main-session supervisor loop around a workflow

```text
launch(A|B) -> wait (never poll) -> read result -> verify diffs -> scoped gate -> full suite
  -> commit named files -> push
```

| Step | Check | Source |
| --- | --- | --- |
| Verify diff | `rtk git diff --stat`; only the run's packages; revert out-of-scope hunks | `../SKILL.md` → Fan-in |
| Re-run red gates | a red package or `fixed` with status not done is re-dispatched by hand | `07f86477` |
| Full suite | `CLAUDE.md` hard rule 5 at the authorized release boundary | `16d36662` |
| Commit | `git commit -- <paths>`, named files, no AI footer; merge, `make install` from a clean tree, push | CLAUDE.md rules 3, 5 |
| Log | decisions and reverts to `reports/DATE-decisions.md` | `claude-code memory report` |

## F. code-analyzer partition: scan, plan, waves

Use when a fix touches several packages and merge conflicts cost more than the serial time. After Refute, one
`depscan` agent per package returns `{files, exported, importers, hotspots}`; a script (no agent) builds a conflict
graph (shared file, or an exported change imported by the other package), colours it greedily into waves of at most
`cap` (default 8, never over the dispatch cap, `rules/harness/hooks.md` → (dispatch cap)), and pulls hotspot files
(`HOTSPOTS` plus `args.hotspots`) into one final single-owner task. Waves run in sequence, tasks of a wave in
parallel; each fixer owns only its listed files.
`fix:false, partition:true` returns `partition {waves, hotspotTask, edges}` and a `## Tasks` plan block, no edits.

Waves, not `isolation: 'worktree'`: no worktree without the owner's yes (`rules/standards/worktree-git.md`), and waves
give the same disjointness with no per-writer setup or merge.

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

- Script in `~/.claude/workflows/<name>.js`; `meta` is a pure literal with `name`, `description`,
  `whenToUse` (USE / CALL / OPTIONAL / RETURNS) and `phases`.
- A row in the table of [saved-workflows.md](saved-workflows.md); `claude-code workflow lint` fails on a script
  missing from it or a row naming no script.
- Copy the `stepOf` block (`// ── stepOf:` to `// ── end stepOf ──`) from `~/.claude/workflows/code-looper.js`, the
  canonical file; after editing it run `claude-code workflow sync ~/.claude/workflows/code-looper.js`; every stage
  reads its pin through it.
- Every `agent()` call pins `model:` (an `agentType:` alone does not count) and `effort:`; external-stage overrides
  follow `saved-workflows.md`.
- Return the shared result shape above; `ERR(...)` for bad args; no shell state outside agents; plain JavaScript.
- A log line with the agent estimate at launch, and a `slice` arg for the first run.
- Run once on a small slice, then update the table and `whenToUse`.
