# Saved Workflows (`workflows/*.js`)

Run one only when the user asks; the default dispatch is `/code-workers` ‖ `copilot`
(`rules/workflows/master-workflow.md` → Phase 6). Each is a deterministic template of master-workflow phases, run by
name (`Workflow({name, args})`) or nested via `workflow(name, args)` (one level). Control flow is code, never a model
decision; agents work in the shared tree.

| Workflow | Shape | Key args (`steps` = per-stage `{agent?, model?, effort?, profile?}`) |
| --- | --- | --- |
| `green-keeper-loop` | gate → fix in scope → re-run, ≤3 per call (whole task ≤5); exits `green\|stalled\|capped\|error` | `{scope, gate?, maxIterations?, steps: {detect, fix}}` |
| `code-engineer` | parallel intel → implementer → copilot repairs existing tests → green-keeper-loop | `{task, scope, packages?, tests?, steps: {intel, impl, test, gate}}` |
| `code-analyzer` | scoped auditors → refute → fix + test per package → gate → re-audit (≤rounds) | `{range\|paths\|findings, fix?, minFix?, rounds?, steps: {scope, audit, refute, fix, test, gate}}` |
| `code-agent` | one external agent as a stage: smoke → JSON envelope → sonnet fallback | `{task, cwd, agent?, model?, vendorModel?, profile?, tools?}` |
| `code-evaluator` | trap scenarios with vs without a skill, then judges score each run | `{base, skill, scenarios?, seeds?, steps: {run, judge}}` |

- A Claude stage runs at `model` = `haiku|sonnet|opus`; an external stage (`copilot`, `codex`, `gemini`, `opencode`)
  goes through `code-agent` with a vendor model id (`gpt-6.1-sol`, `kimi-k3`, `discovery`). Unknown stages error before any
  agent runs. Parser: `stepOf` in `workflows/code-agent.js`.
- Under Claude quota pressure (`low`/`exhausted` from `claude-code gateway router --agent claude`), the scripts step
  their own Claude pins down one alias (opus→sonnet→haiku), fail-open.

## Authoring

- Scripts stay in `workflows/` (the only place by-name runs resolve); `claude-code workflow lint` fails when a script
  is missing from the table above or a row names no script.
- Every `agent()` call pins `model:` (or `agentType:`) and `effort:`; `meta` is a pure literal; plain JavaScript only.

## Run from a leaf

A leaf has no `Workflow` tool, so it runs one through a headless Claude session, only when its brief names it:

```bash
claude-code ask --agent claude --tools --timeout 20m "Run Workflow({name: \"<name>\", args: {…}}) and print the result"
```

`--background` past ~3 minutes; from a scratch cwd pass the script inline (`Workflow({script: <contents>})`). Put the
job id on the `Ran on:` line and report the result as a claim, not as the leaf's own verification.
