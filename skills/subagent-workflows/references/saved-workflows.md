# Saved Workflows (`workflows/*.js`)

Run one only when the user asks; the default dispatch is `claude` ‖ `copilot`
(`rules/workflows/master-workflow.md` → Phase 6). Each is a deterministic template of master-workflow phases, run by
path (`Workflow({scriptPath: "~/.claude/workflows/<name>.js", args})`; a by-name call is accepted but `dispatch-effort`
redirects it to the `scriptPath`) or nested via `workflow(name, args)` (one level). Control flow is code, never a model
decision; agents work in the shared tree.

| Workflow | Shape | Key args (`steps` = `{agent?, model?, effort?, profile?, provider?}`; args `profile?`, `provider?`) |
| --- | --- | --- |
| `green-keeper-loop` | the gate: detect (or `gate`) → fix in scope → re-run, ≤3 per call (whole task ≤5); wraps `stageDetect`/`stageGate`; exits `green\|stalled\|capped\|error` | `{scope, gate?, maxIterations?, fixesPerIteration?, claudeState?, steps: {detect, fix}}` |
| `code-analyzer` | scoped auditors → proximity merge (±10 lines or same symbol) → refute → dependency scan + wave plan → fix + test per package, wave by wave (hotspot files last) → gate (in-script `stageGate`, no nested workflow) → re-audit (≤rounds); the range audit entry, no polish; returns the shared code-workers shape (no `polish`/`files`) plus refuted, rounds | `{range\|paths\|findings, fix?, partition?, cap?, hotspots?, minFix?, rounds?, dimensions?, focus?, tests?, gate?, maxIterations?, prior?, chunkChars?, refuteBatch?, steps: {scope, audit, refute, fix, test, gate, depscan}}` |
| `code-evaluator` | the judge: trap scenarios with vs without a skill, then judges score each run; standalone, or routed from `code-engineer` `skill`; returns `{runs, cells}` | `{base, skill, scenarios?, seeds?, conditions?, steps: {run, judge}}` |
| `code-workers` | per-package repair, streaming, no barrier: find → rederive (each finding carries `kind`: `bug` needs a derived failing scenario, a cost — `perf\|runtime\|complexity\|test-quality` — needs the cost cited in code and a behavior-keeping fix; a finder/rederiver severity split across the fix line gets one blind tie-breaker; median of three) → fix → guard test → gate (`gate` = one command for every package, e.g. `npm test` for JS/TS, default `go build/vet/test ./<pkg>/...`; a red gate gets fixer iterations, `maxIterations` default 2, 0 = check only); then ONE polish over the merged diff after the stream: Dedupe (2+ packages fixed), Simplify, Review, and Fix-all (every finding, lows included) → guard test → re-Gate per package with findings (`polish:false`, or under 150k budget tokens, skips it; a red re-gate, unfinished fix-all or MEDIUM+ finding outside the fixed packages is red); `polish:false` also hands findings cited outside its packages back in `foreign` instead of repairing them; `known [{file, symptom}]` treats a named bug as confirmed and skips Find and Rederive for its package (`fast:false` disables); exits `green\|red\|review\|error`; returns `polish`, `files`, `foreign` | `{cwd, packages\|paths, slice?, fix?, minFix?, focus?, known?, fast?, maxIterations?, gate?, polish?, steps: {find, rederive, fix, test, gate, dedupe, simplify, review}}` |
| `code-engineer` | wrapper: size the job (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) then route: the target's top-level dirs split into ≤3 file-disjoint groups, one `code-workers` child each (`polish:false`), all in parallel (MEDIUM adds intel, a feature adds an implementer); `gate` + `gateScope` (dirs disjoint from the groups) adds one `green-keeper-loop` child beside them (none under `fix:false`), and the shared gate is re-checked once, check-only, after every child and the polish (red → red); `gate` without `gateScope` is every package's gate instead, passed to the `code-workers` children; findings a child cites outside its group come back to code-engineer, which repairs them once after the children, one dir at a time; `skill` alone → `code-evaluator` (returns `runs`, `cells`); a bare `range` → `code-analyzer` scan, then `code-workers` on its findings (with a task, paths or packages its changed Go files become paths); EPIC drafts a plan; routing picks the path: a fix task goes straight to code-workers (the common path); an audit, a package list with no task or a bare range first runs code-analyzer review-only and hands its confirmed findings to code-workers as `known` (`scan:false` skips); `skill` runs code-evaluator first; green-keeper-loop loops over dirs whose gate stayed red and over every touched dir when 2+ children ran (`loop:false` skips); then polish once over the merged diff (`polish:false` or under 150k budget tokens skips): Dedupe (2+ packages), Simplify, Review, one Fix-all + guard test + gate per package with findings, no second child run; a LOW/MEDIUM fix task skips Find and Rederive via `known` unless `fast:false`; a final Record agent files the report and decisions (`record:false` skips); returns `files` (sorted repo-relative files the implementer, fixers, gate fixers and polish changed), `polish`; exits `green\|red\|review\|plan\|error`, or a child's `blocked\|stalled\|incomplete\|capped` kept as-is | `{cwd, task\|packages\|paths\|range\|skill, kind? (feature\|refactor\|fix\|audit; wins over sizing and the task-verb guess; feature/refactor get an implementer, incomplete when it changes no file, review when it reports no live run), tier?, fix?, minFix?, focus?, fast?, polish?, gate?, gateScope?, base?, scenarios?, seeds?, conditions?, label?, record?, scan?, loop?, scripts?, dimensions? (code-analyzer lenses), steps: {size, intel, impl, plan, dedupe, simplify, review, record, child}}` |

- A stage runs on `agent` = `claude` (in-context on the session profile, `model` = `haiku|sonnet|opus|fable`) or
  `codex|copilot|gemini|opencode`: a `sonnet` leaf of that type runs
  `claude-code <a> ask --fallback agents --format json [--profile <p>] [--provider <x>]`, and the stage reports
  `ran on` (with hop count) or BLOCKED (`coworkers` rows in the result). The default is the agent's pin;
  `profile`/`provider` (run args or per stage) are the Databricks-arm flag and lead instead.
  After a 429/quota/auth failure, even mid-run, the chain runs: same model on another endpoint, GitHub copilot
  gpt-6.1-sol, another agent, BLOCKED; an editing successor checks `git diff` first. gemini defaults to Google; a
  named Databricks profile or the arm runs it there. A run preflights each external route once with
  `claude-code gateway`.
  Parser: `stepOf`, verbatim in `code-analyzer`, `green-keeper-loop`, `code-evaluator`, `code-workers` and
  `code-engineer` (enforced by `workflow lint`).
- Stages: the `// ── stages:` block (`stageGate`, `stageDetect`, `stageFix`, `stagePolish`), verbatim in
  `code-analyzer`, `green-keeper-loop`, `code-workers` and `code-engineer` (enforced by `workflow lint`). Stages run
  in-script, so a workflow nests one level only. Shell prompts carry no `cd … &&` prefix: the agent's cwd is already
  `cwd`.
- `code-engineer` is the size-routed rewrite, renamed from `orchestrate` on 2026-11-07 (A/B:
  `memory/claude-code/reports/2026-11-07-workflow-trial-decisions.md`).
- Under Claude quota pressure (`low`/`exhausted` from `claude-code gateway --agent claude`), the scripts step
  their own Claude pins down one alias (opus→sonnet→haiku), fail-open. `green-keeper-loop` takes `claudeState`
  (`ok|low|exhausted|unknown`) from its caller or reads it itself; `code-analyzer` runs its gate in-script and passes
  none.

## Authoring

- Scripts stay in `workflows/` (the only place by-name runs resolve; the documented call is `scriptPath`);
  `claude-code workflow lint` fails when a script is missing from the table above or a row names no script.
- `workflow lint` compiles every script with node and fails one that does not parse; no node prints
  `parse check skipped`.
- Every `agent()` call pins `model:` (or `agentType:`) and `effort:`; `meta` is a pure literal; plain JavaScript only.

## Run from a leaf

A leaf has no `Workflow` tool, so it runs one through a headless Claude session, only when its brief names it:

```bash
claude-code agents ask --agent claude --tools --timeout 20m \
  "Run Workflow({scriptPath: \"~/.claude/workflows/<name>.js\", args: {…}}) and print the result"
```

`--background` past ~3 minutes; from a scratch cwd pass the script inline (`Workflow({script: <contents>})`). Put the
job id on the `Ran on:` line and report the result as a claim, not as the leaf's own verification.
