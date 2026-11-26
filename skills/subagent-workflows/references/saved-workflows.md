# Saved Workflows (`workflows/*.js`)

Run one only when the user asks. Use `Workflow({scriptPath: "~/.claude/workflows/<name>.js", args})`;
nested `workflow(name, args)` is limited to one level. Control flow belongs to the script.
Tree selection: `rules/standards/worktree-git.md` → Worktrees.
Validation: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.

| Workflow | Purpose | Primary args |
| --- | --- | --- |
| `code-looper` | Repair a bounded failing check. | `scope\|files`, `gate\|gates`, `known`, `maxIterations` |
| `code-analyzer` | Audit, refute and partition a range. | `range\|paths\|packages`, `fix`, `partition` |
| `code-evaluator` | Compare skill/route conditions on the same scenarios. | `base`, `skill`, `scenarios`, `seeds` |
| `code-workers` | Find, rederive and repair scoped findings. | `packages\|paths`, `known`, `slice`, `fix` |
| `code-engineer` | Size and route a task; collect its report. | `task\|packages\|paths\|range\|skill\|plan`, `tier` |

Read the selected script's `meta.whenToUse` and argument parser for its full schema; do not copy schemas into briefs.
Current repair scripts still contain package suites and child polish/review stages. Those defaults do not implement
the collector pipeline; use compatible scoped checks and keep final sign-off with main. Do not treat script
defaults as an exception to the validation rule.

- Pass complete findings between stages: file, line, symbol, symptom, scenario and evidence. `known` skips duplicate
  discovery; `fast:false` restores it. A blind re-deriver sees the claim, not the finder's reasoning.
- `code-analyzer` with `fix:false, partition:true` returns a plan for `agents ask --waves`.
  `code-engineer` can accept that plan directly; its runner lifecycle is in the script.
- Routing uses `steps` per-stage overrides and run-wide `profile`/`provider`; schema and inheritance:
  `rules/harness/agent.md` → Overlay. Inspect `stepOf` for accepted stage agents.
- `stepOf` and the stage blocks come from `internal/assets/workflows/blocks/`; prompts come from
  `internal/assets/workflows/prompts/`. Rebuild, then `claude-code workflow sync` projects marked blocks.
- By-name calls and scripts with missing/wrong-case pins may be rewritten inline by `dispatch-effort`;
  the source file stays unchanged. A routed stage reports the work model or BLOCKED; never infer it from its wrapper.
- Stage measurements use `agents diff --base HEAD --path <p> --format json` snapshots, not the agent's line claims.
  Keep production/test deltas per stage, package and total in the report.
- `lineBudget`: `shrink` is production net ≤ 0; `neutral` ≤ +20; `free` is unlimited. Defaults follow `kind`.
  One shrink-or-justify pass follows excess; persistent excess is reported, never auto-reverted.
  Dedupe and Simplify keep net ≤ 0 or report partial with the measured excess.
- `kind: consolidate` merges duplicates and removes dead paths while preserving behavior, messages and exit codes.
- Test selection and proposals: `rules/standards/testing-guidelines.md` → What gets a test, and what never does.
- Only observed checks establish a green result. A fixer's claim or Test stage status does not establish completion.
  Compare confirmed and fixed findings; keep foreign findings and cross-package seams for their owner.
- Scripts validate unknown args, stages and stage fields at launch. Check explicit model choices against any
  quota-driven pin adjustment before running; preserve owner choices per `../SKILL.md` → Rules.

## Authoring

- Keep scripts in `workflows/`; every script needs a row above and every row a script.
- Pin `model` and `effort` on every `agent()`; `agentType` alone is not a pin.
- Keep `meta` a pure literal, use plain JavaScript and `ERR(...)` for bad arguments, not a top-level throw.
- Embed the marked `stepOf` and stage blocks verbatim; `workflow sync` updates them.
- Run `claude-code workflow lint` for table drift, pins, blocks and Node parse checks.
- Start with a small `slice`; report measured results and update the table and `whenToUse`.

## Run from a leaf

A leaf has no `Workflow` tool. Only when its brief names one, ask a headless Claude session to run it:

```bash
claude-code agents ask --background --overlay '{"agent":"claude","tools":true}' \
  'Run Workflow({scriptPath: "~/.claude/workflows/<name>.js", args: {…}}) and print the result'
```

From scratch cwd, pass script contents inline. Waiting, receipts and budget-ended work:
`../SKILL.md` → External relay contract. Report the workflow's result as a claim.
`memory feedback show <id>` prints a candidate call; main runs it after owner approval, then records completion
with `memory feedback close <id> <sha|file:line>`.

## Fan-in and run rules

- Inspect owned-file changes and compare confirmed findings with fixes; report dropped work and dispatch failures.
  Ownership and recovery: `../SKILL.md` → Fan-in and Error handling.
- Keep the main shell at the repo root. Do not edit the workflow scripts while they are running.
- Rename an evaluation condition in its runbook and fixture directories together.
- Collection and final sign-off follow the two rules linked at the top; workflow status does not replace them.
