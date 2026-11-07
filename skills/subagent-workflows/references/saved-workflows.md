# Saved Workflows (`workflows/*.js`)

Run one only when the user asks; the default dispatch is `/code-worker` ‖ `copilot`
(`rules/workflows/master-workflow.md` → Phase 6). Each is a deterministic template of master-workflow phases, run by
path (`Workflow({scriptPath: "~/.claude/workflows/<name>.js", args})`; a by-name call is accepted but `dispatch-effort`
redirects it to the `scriptPath`) or nested via `workflow(name, args)` (one level). Control flow is code, never a model
decision; agents work in the shared tree.

| Workflow | Shape | Key args (`steps` = per-stage `{model?, effort?}`) |
| --- | --- | --- |
| `green-keeper-loop` | gate → fix in scope → re-run, ≤3 per call (whole task ≤5); exits `green\|stalled\|capped\|error` | `{scope, gate?, maxIterations?, fixesPerIteration?, claudeState?, steps: {detect, fix}}` |
| `code-analyzer` | preflight probe of `code-worker:claude` (fix on; missing plugin agents → `exit: error`, restart the session) → scoped auditors → proximity merge (±10 lines or same symbol) → refute → dependency scan + wave plan → fix + test per package, wave by wave (hotspot files last) → gate → re-audit (≤rounds); returns the shared relay3 shape plus refuted, rounds | `{range\|paths\|findings, fix?, partition?, cap?, hotspots?, minFix?, rounds?, dimensions?, tests?, gate?, maxIterations?, prior?, chunkChars?, refuteBatch?, steps: {scope, audit, refute, fix, test, gate, depscan}}` |
| `code-evaluator` | trap scenarios with vs without a skill, then judges score each run | `{base, skill, scenarios?, seeds?, conditions?, steps: {run, judge}}` |
| `relay3` | streaming per-package relay: find → rederive (a finder/rederiver severity split across the fix line gets one blind tie-breaker; median of three) → fix → guard test → gate, then per package Simplify → Review → Fix-all (every finding, lows included) → guard test → re-Gate (one cycle; `polish:false` or under 150k budget tokens skips; a red re-gate or unfinished fix-all is red), no barrier between packages; `known [{file, symptom}]` treats a named bug as confirmed and skips Find and Rederive for its package (`fast:false` disables); exits `green\|red\|review\|error` | `{cwd, packages\|paths, slice?, fix?, minFix?, focus?, known?, fast?, polish?, steps: {find, rederive, fix, test, gate, simplify, review}}` |
| `code-engineer` | size the job (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) then route: relay3 on paths (MEDIUM adds intel, a feature adds an implementer), relay3 on packages (HIGH), plan draft (EPIC); when relay3 fixed something, a final pass over the merged diff: Dedupe (only when 2+ packages were fixed), Simplify, Review (`polish:false` skips both this and relay3's per-package chain; a LOW/MEDIUM fix task skips relay3's Find and Rederive via `known` unless `fast:false`; the final pass and the loop are skipped under 150k budget tokens; any review finding, lows included, earns one more relay3, capped at one loop); a final Record agent files the report and decisions (`record:false` skips); `range` = a git range whose changed Go files become paths; exits `green\|red\|review\|plan\|error` | `{cwd, task\|packages\|paths\|range, tier?, fix?, minFix?, focus?, fast?, polish?, label?, record?, steps: {size, intel, impl, plan, dedupe, simplify, review, record, child}}` |

- Workflows are Claude only: a stage runs at `model` = `haiku|sonnet|opus|fable` (`effort` up to `max`), and an
  `agent`/`profile` field or an unknown stage errors before any agent runs. Parser: `stepOf`, copied verbatim into
  `code-analyzer`, `green-keeper-loop`, `code-evaluator`, `relay3` and `code-engineer`. External agents
  run outside workflows (`/code-worker:codex`, `/coworker:copilot`, `claude-code agents ask`).
- `code-engineer` is the size-routed rewrite, renamed from `orchestrate` on 2026-11-07 (A/B:
  `memory/claude-code/reports/2026-11-07-workflow-trial-decisions.md`).
- Under Claude quota pressure (`low`/`exhausted` from `claude-code gateway router --agent claude`), the scripts step
  their own Claude pins down one alias (opus→sonnet→haiku), fail-open. The top-level script reads it once and passes
  it down as `claudeState` (`ok|low|exhausted|unknown`); a failed read passes `unknown`. Nested `green-keeper-loop`
  calls skip their own read when it is present. Run directly, they read it themselves.

## Authoring

- Scripts stay in `workflows/` (the only place by-name runs resolve; the documented call is `scriptPath`);
  `claude-code workflow lint` fails when a script is missing from the table above or a row names no script.
- Every `agent()` call pins `model:` (or `agentType:`) and `effort:`; `meta` is a pure literal; plain JavaScript only.

## Run from a leaf

A leaf has no `Workflow` tool, so it runs one through a headless Claude session, only when its brief names it:

```bash
claude-code agents ask --agent claude --tools --timeout 20m \
  "Run Workflow({scriptPath: \"~/.claude/workflows/<name>.js\", args: {…}}) and print the result"
```

`--background` past ~3 minutes; from a scratch cwd pass the script inline (`Workflow({script: <contents>})`). Put the
job id on the `Ran on:` line and report the result as a claim, not as the leaf's own verification.
