# Saved Workflows (`workflows/*.js`)

Run one only when the user asks; the default dispatch is `/code-worker` ‖ `copilot`
(`rules/workflows/master-workflow.md` → Phase 6). Each is a deterministic template of master-workflow phases, run by
name (`Workflow({name, args})`) or nested via `workflow(name, args)` (one level). Control flow is code, never a model
decision; agents work in the shared tree.

| Workflow | Shape | Key args (`steps` = per-stage `{model?, effort?}`) |
| --- | --- | --- |
| `green-keeper-loop` | gate → fix in scope → re-run, ≤3 per call (whole task ≤5); exits `green\|stalled\|capped\|error` | `{scope, gate?, maxIterations?, claudeState?, steps: {detect, fix}}` |
| `code-analyzer` | scoped auditors → proximity merge (±10 lines or same symbol) → refute → fix + test per package → gate → re-audit (≤rounds); returns the shared relay3 shape plus refuted, rounds | `{range\|paths\|findings, fix?, minFix?, rounds?, steps: {scope, audit, refute, fix, test, gate}}` |
| `code-evaluator` | trap scenarios with vs without a skill, then judges score each run | `{base, skill, scenarios?, seeds?, steps: {run, judge}}` |
| `relay3` | streaming per-package relay: find → rederive → fix → guard test → gate, no barrier between packages; exits `green\|red\|review\|error` | `{cwd, packages\|paths, fix?, minFix?, focus?, steps: {find, rederive, fix, test, gate}}` |
| `orchestrate` | size the job (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) then route: relay3 on paths (MEDIUM adds intel, a feature adds an implementer), relay3 on packages (HIGH), plan draft (EPIC); exits `green\|red\|review\|plan\|error` | `{cwd, task\|packages\|paths, tier?, fix?, minFix?, focus?, steps: {size, intel, impl, plan, child}}` |

- Workflows are Claude only: a stage runs at `model` = `haiku|sonnet|opus|fable` (`effort` up to `max`), and an
  `agent`/`profile` field or an unknown stage errors before any agent runs. Parser: `stepOf`, copied verbatim into
  `code-analyzer`, `green-keeper-loop`, `code-evaluator`, `relay3` and `orchestrate`. External agents
  run outside workflows (`/code-worker:codex`, `/coworker:copilot`, `claude-code agents ask`).
- `code-engineer` is retired; `orchestrate` replaces it (A/B:
  `memory/claude-code/reports/2026-11-07-workflow-trial-decisions.md`).
- Under Claude quota pressure (`low`/`exhausted` from `claude-code gateway router --agent claude`), the scripts step
  their own Claude pins down one alias (opus→sonnet→haiku), fail-open. The top-level script reads it once and passes
  it down as `claudeState` (`ok|low|exhausted|unknown`); a failed read passes `unknown`. Nested `green-keeper-loop`
  calls skip their own read when it is present. Run directly, they read it themselves.

## Authoring

- Scripts stay in `workflows/` (the only place by-name runs resolve); `claude-code workflow lint` fails when a script
  is missing from the table above or a row names no script.
- Every `agent()` call pins `model:` (or `agentType:`) and `effort:`; `meta` is a pure literal; plain JavaScript only.

## Run from a leaf

A leaf has no `Workflow` tool, so it runs one through a headless Claude session, only when its brief names it:

```bash
claude-code agents ask --agent claude --tools --timeout 20m \
  "Run Workflow({name: \"<name>\", args: {…}}) and print the result"
```

`--background` past ~3 minutes; from a scratch cwd pass the script inline (`Workflow({script: <contents>})`). Put the
job id on the `Ran on:` line and report the result as a claim, not as the leaf's own verification.
