# Saved Workflows (`workflows/*.js`)

Run one only when the user asks; the default dispatch is `claude` ‖ `copilot` (`../SKILL.md` → Test agent). Each
is a deterministic template of master-workflow phases, run by path
(`Workflow({scriptPath: "~/.claude/workflows/<name>.js", args})`; a by-name call is accepted but `dispatch-effort`
redirects it to the `scriptPath`) or nested via `workflow(name, args)` (one level). Control flow is code, never a
model decision; agents work in the shared tree.

| Workflow | Shape | Key args (`steps` = `{agent?, model?, effort?, profile?, provider?}`, checked and route-previewed by the `dispatch-effort` hook before the run; args `profile?`, `provider?`) |
| --- | --- | --- |
| `code-looper` | the gate: detect (or `gate`) → fix in scope → re-run, ≤3 per call (whole task ≤5); wraps `stageDetect`/`stageGate`, the detect leaf doubling as the read-only check (green only when the check re-runs it green); exits `green\|stalled\|capped\|error`; returns `lines`, `gates` | `{scope, gate?, maxIterations?, fixesPerIteration?, claudeState?, steps: {detect, fix}}` |
| `code-analyzer` | scoped auditors → proximity merge (±10 lines or same symbol) → refute → dependency scan + wave plan → fix + test repair per package, wave by wave (hotspot files last) → gate (in-script `stageGate`, check-confirmed, no nested workflow) → re-audit (≤rounds); `kind: consolidate` swaps the lenses for duplicates, dead code, legacy and stale tests; `red` is a package whose fix is not done or whose last gate is not green; the range audit entry, no polish; returns the shared code-workers shape (no `polish`/`files`) plus refuted, rounds | `{range\|paths\|findings, fix?, partition?, cap?, hotspots?, minFix?, rounds?, dimensions?, focus?, tests?, gate?, maxIterations?, prior?, chunkChars?, refuteBatch?, kind?, lineBudget?, maxAgents?, steps: {scope, audit, refute, fix, test, gate, depscan}}` |
| `code-evaluator` | the judge: trap scenarios with vs without a skill, then judges score each run; standalone, or routed from `code-engineer` `skill`; returns `{runs, cells}` | `{base, skill, scenarios?, seeds?, conditions?, steps: {run, judge}}` |
| `code-workers` | per-package repair, streaming, no barrier: find (`kind: consolidate`: duplicate, dead, legacy and stale-test lenses, merge-and-delete fixes) → rederive (each finding carries `kind`: `bug` needs a derived failing scenario, a cost — `perf\|runtime\|complexity\|test-quality` — needs the cost cited in code and a behavior-keeping fix; a finder/rederiver severity split across the fix line gets one blind tie-breaker; median of three) → fix → test repair → gate (`gate` = one command for every package, e.g. `npm test` for JS/TS, default `go build/vet/test ./<pkg>/...`; a red gate gets fixer iterations, `maxIterations` default 2, 0 = check only); then ONE polish over the merged diff after the stream: Dedupe (2+ packages fixed), Simplify, Review, and Fix-all (every finding, lows included) → test repair → re-Gate per package with findings, plus one gate over polished packages no Fix-all re-gated (`polish:false`, or under 150k budget tokens, skips it; a red re-gate, unfinished fix-all or MEDIUM+ finding outside the fixed packages is red); `polish:false` also hands findings cited outside its packages back in `foreign` instead of repairing them, and exported-signature or cross-package needs back in `seams`; under `provider: databricks` with no `profile`, packages take `profiles` (default `dev`, `qa`, `prod`) round-robin; `known [{file, symptom}]` treats a named bug as confirmed and skips Find and Rederive for its package (`fast:false` disables); exits `green\|red\|review\|error`; returns `polish`, `files`, `foreign`, `seams` | `{cwd, packages\|paths, slice?, fix?, minFix?, focus?, known?, fast?, maxIterations?, gate?, polish?, kind?, lineBudget?, profiles?, steps: {find, rederive, fix, test, gate, dedupe, simplify, review}}` |
| `code-engineer` | wrapper: size the job (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) then route: one `code-workers` child (`polish:false`) per top-level dir, launched in waves of `cap` (default 16, max 24), with one serial Seam step after each wave applying the exported-signature and cross-package changes the children reported in `seams`; under `provider: databricks` with no `profile` the children take `profiles` (default `dev`, `qa`, `prod`) round-robin (MEDIUM adds intel, a feature adds an implementer and ONE test); `gate` + `gateScope` (dirs disjoint from the groups) adds one `code-looper` child beside them (none under `fix:false`), and the shared gate is re-checked once, check-only, after every child and the polish (red → red); `gate` without `gateScope` is every package's gate instead, passed to the `code-workers` children; findings a child cites outside its group come back to code-engineer, which repairs them once after the children, one dir at a time; `skill` alone → `code-evaluator` (returns `runs`, `cells`); a bare `range` → `code-analyzer` scan, then `code-workers` on its findings (with a task, paths or packages its changed Go files become paths); EPIC drafts a plan; routing picks the path: a fix task goes straight to code-workers (the common path); an audit, a package list with no task or a bare range first runs code-analyzer review-only and hands its confirmed findings to code-workers as `known` (`scan:false` skips); `skill` runs code-evaluator first; code-looper loops over dirs whose gate stayed red, every touched dir no green gate covers yet, and every touched dir when 2+ children ran (`loop:false` skips); then polish once over the merged diff (`polish:false` or under 150k budget tokens skips): Dedupe (2+ packages), Simplify, Review, one Fix-all + test repair + gate per package with findings, one gate over polished packages no Fix-all re-gated, no second child run; a LOW/MEDIUM fix task skips Find and Rederive via `known` unless `fast:false`; a final Record agent files the report and decisions (`record:false` skips); returns `files` (sorted repo-relative files the implementer, fixers, gate fixers and polish changed), `polish`; exits `green\|red\|review\|plan\|error` (red = any touched or gated dir whose last gate is not green), or a child's `blocked\|stalled\|incomplete\|capped` kept as-is | `{cwd, task\|packages\|paths\|range\|skill, kind? (feature\|refactor\|fix\|audit\|consolidate; wins over sizing and the task-verb guess; feature/refactor get an implementer, incomplete when it changes no file, review when it reports no live run), tier?, fix?, minFix?, focus?, fast?, polish?, gate?, gateScope?, base?, scenarios?, seeds?, conditions?, label?, record?, scan?, loop?, scripts?, dimensions? (code-analyzer lenses), lineBudget?, cap?, profiles?, steps: {size, intel, impl, plan, dedupe, simplify, review, record, child}}` |

- A stage runs on `agent` = `claude` (in-context, session profile, `model` = `haiku|sonnet|opus|fable`) or
  `codex|copilot|gemini|opencode`: a `sonnet` leaf of that type runs
  `claude-code <a> ask --fallback agents --format json [--profile <p>] [--provider <x>]`, and the stage reports
  `ran on` (with hop count) or BLOCKED (`coworkers` rows in the result). The default is the agent's pin;
  `profile`/`provider` (run args or per stage) become `--profile`/`--provider`: the lead profile, or that
  provider's signed-in profile.
  After a 429/quota/auth failure, even mid-run, the `--fallback agents` order runs (the requested target, then the
  agent's pin), then BLOCKED ([cost-routing.md](cost-routing.md) → Fallback); an editing successor checks `git diff`
  first. gemini defaults to Google; a named Databricks profile or the arm runs it there. A run preflights each
  external route once with `claude-code gateway`.
  Parser: `stepOf`, verbatim in `code-analyzer`, `code-looper`, `code-evaluator`, `code-workers` and
  `code-engineer` (enforced by `workflow lint`).
- Stages: the `// ── stages:` block (`stageGate`, `stageDetect`, `stageFix`, `stagePolish`), verbatim in
  `code-analyzer`, `code-looper`, `code-workers` and `code-engineer` (enforced by `workflow lint`). Stages run
  in-script, so a workflow nests one level only. Shell prompts carry no `cd … &&` prefix: the agent's cwd is already
  `cwd`.
- Measured, not reported: after every editing stage (fix, shrink, test, gate iterations, implementer, seam, Dedupe,
  Simplify) a local haiku leaf reads `git diff HEAD --numstat` plus untracked files (`stSnap`), and the script
  subtracts the snapshot before it (`stDelta`): test lines are `_test.go`, `.test.js`/`.test.ts`, `tests/` or
  `testdata/` paths. The result's `lines` holds every stage row, `byPkg` and `total` (production vs test); the report
  ends with the table. An agent's own `deltaLines` is never used.
- `lineBudget` (`code-engineer`, `code-workers`, `code-analyzer`): `shrink` = production net ≤ 0 per package,
  `neutral` = ≤ +20, `free` = no limit; the default follows `kind` (`refactor` neutral, `consolidate` shrink, else
  free). An over-budget fix (or implementer) gets ONE shrink-or-justify pass; still over, the package lands in `over`
  and a green run exits `review`. Files are never auto-reverted. Dedupe and Simplify are always held to measured
  net ≤ 0 and report `partial` with the number otherwise.
- `kind: consolidate`: the find stage looks for duplicates, dead code, legacy paths and stale or redundant tests (not
  bugs); the fix stage merges and deletes, with behaviour, messages and exit codes unchanged.
- Test stage (`testing-guidelines.md` → A test must guard): repair the tests the edits broke; `kind: feature` adds ONE
  test for the feature (in `code-engineer`, after the implementer); every other test is returned in `proposedTests`
  (`{pkg, case, reason, runtime}`) and listed in the report for the owner to approve. Any test written must run in
  milliseconds: no sleep, real network, vault, daemon or spawned binary.
- Exit from gates: every `stageGate` result lands in `gates` in completion order (children's included); a run is green
  only when the last gate covering each touched or gated dir is green (`stGateRed`). A Test stage's status or a
  fixer's own green never decides it.
- Arguments: an unknown top-level arg, an unknown `steps` stage (`steps.child` included) or an unknown stage field is
  refused at launch with an error naming it. `profile` reaches every child and every external stage's
  `--profile`; with `--fallback agents` a failed lead route can still fall back to the agent's pin (`ran on` shows it).
- Under Claude quota pressure (`low`/`exhausted` from `claude-code gateway --agent claude`), the scripts step
  their own Claude pins down one alias (opus→sonnet→haiku), fail-open. `code-looper` takes `claudeState`
  (`ok|low|exhausted|unknown`) from its caller or reads it itself; `code-analyzer` runs its gate in-script and passes
  none.

## Authoring

- Scripts stay in `workflows/` (the only place by-name runs resolve; the documented call is `scriptPath`);
  `claude-code workflow lint` fails when a script is missing from the table above or a row names no script.
- `workflow lint` compiles every script with node and fails one that does not parse; no node prints
  `parse check skipped`.
- Every `agent()` call pins `model:` (or `agentType:`) and `effort:`; `meta` is a pure literal; plain JavaScript only.

## Run from a leaf

A leaf has no `Workflow` tool, so it runs one through a headless Claude session, only when its brief names it.
Off Anthropic that child's gated tool set has no `Workflow` tool (`internal/claude/claude.go` `gateTools`), so run
it on Anthropic or from the main session:

```bash
claude-code agents ask --agent claude --tools --timeout 20m \
  "Run Workflow({scriptPath: \"~/.claude/workflows/<name>.js\", args: {…}}) and print the result"
```

`--background` past ~3 minutes; from a scratch cwd pass the script inline (`Workflow({script: <contents>})`). Put the
job id on the `Ran on:` line and report the result as a claim, not as the leaf's own verification.

## Fan-in and run rules

- **Output is a claim:** diff it against the brief's file list (revert or park outside edits), rerun the gate of its
  last-touched package, compare confirmed findings to fixed ones (a gap = dropped findings, rerun). A red run caused
  only
  by a `blocked` stage (classifier down) is not a code failure; a stage refusing a change because behavior would differ
  is right — accept the partial landing.
- **While it runs:** package-scoped gates only (a repo-wide check trips on peers' WIP), main shell at the repo root,
  never let it edit the `workflows/*.js` it runs from (fix afterwards with a leaf), rename a condition in its runbook
  (`eval.md`) and fixture dirs in the same pass.
