# Saved Workflows (`workflows/*.js`)

Run one only when the user asks for it; the default dispatch is `/code-workers` ‖ `copilot`
(`rules/workflows/master-workflow.md` → Phase 6). Deterministic templates of the master-workflow phases, runnable by
name (`Workflow({name, args})`) or inline from another script via `workflow(name, args)`. Sequential = awaited stages,
Parallel = `parallel()`/`pipeline()`, Loop = a bounded `while` — control flow is code, never a model decision. Agents
work in the shared tree.

Placement: a script that another script calls by name, that more than one skill uses, or that the user invokes by
name lives in `workflows/`; otherwise it lives in its owning skill's `scripts/` and runs by `scriptPath`.

The scripts stay in `workflows/`: Claude Code resolves a by-name run and a nested `workflow(name)` there only (read
once at session start), and the `dispatch-effort` hook redirects a by-name call to `workflows/<name>.js`. This file is
their one doc home; `claude-code workflow lint` fails when a script is missing from the table below or a row names a
script that does not exist, so a rename or move cannot go unrecorded.

Every `agent()` call pins `model:` (or `agentType:`) AND `effort:` — an unpinned leaf inherits the session's reasoning
effort; a call that deliberately omits `effort:` carries `// lint:opus-ok` on its closing line. `meta` is a pure literal
and nothing throws at top level, or the registry skips the script silently. Scripts are plain JavaScript (the Workflow
tool runs no TypeScript); `claude-code workflow lint` checks all of the above and `make test` gates the shipped scripts.

`green-keeper-loop`, `code-engineer`, `code-analyzer` and `code-agent` read the Claude account's own router state once at start (`claude-code gateway router --format json
--agent claude --fallback none --model sonnet`, a haiku leaf; they parse `candidates[0].state`, else the `skipped[]`
entry whose `agent` is `claude`; `code-agent` skips the read when a `profile` is named or `fallback` is off); `low`/`exhausted` steps every Claude-stage `model:` pin down one model through an
inline `routeModel()` helper (opus→sonnet, sonnet→haiku, haiku stays haiku), fail-open with a `log()` line on a failed
or null read — pins stay explicit literals so the `dispatch-effort` pin step still passes.

Reorg mode (CLAUDE.md): pass `tests: false` and an explicit build+vet `gate`.

| Workflow | Owner skill | Shape | Phases | args |
|----------|-------------|-------|--------|------|
| `green-keeper-loop` | problem-solving (`../../problem-solving/references/verify.md` Gate Loop) | Loop: gate → fix in scope → re-run, ≤N; exits `green\|stalled\|capped\|error`. Per-call default 3 iterations (this workflow's loop cap; verify.md caps the whole task at 5). The full-suite gate runs here only because the main thread opted in by invoking it. Many repos: one call per repo, in parallel | 8 | `{gate?, cwd?, scope, maxIterations?, fixesPerIteration?}` |
| `code-engineer` | subagent-workflows (the parallel `code-worker` ‖ `copilot` shape) | Parallel intel → implementer → copilot (repairs only) → green-keeper-loop. Scripted exception to `testing-guidelines.md` → Test agent: copilot repairs after the implementer lands. Does not chain into `code-analyzer` | 2, 6, 8 | `{task, cwd?, scope, packages?, gate?, maxIterations?, implModel?, tests?}` — `tests: false` skips the copilot test-repair stage; returns `status` = `blocked\|error\|<gate exit>` |
| `code-analyzer` | evaluate-code (its fix stage follows problem-solving) | Sized scope → chunked auditors → batched refute → fix → test per package → green-keeper-loop → re-audit touched files (≤rounds). Loops rounds in-script (one level of `workflow()` nesting only); exits `dry\|no-fix\|stalled\|nothing-changed\|blocked\|capped\|error` | 2, 6, 8 | `{range \| paths \| findings (none = working-tree diff), cwd?, packages?, dimensions?, chunkChars?, fix?, minFix?, gate?, maxIterations?, rounds?, prior?, refuters?, refuteBatch?, tests?}` — `tests: false` skips the per-package test agent |
| `code-agent` | subagent-workflows (`cost-routing.md`) | One codex/gemini/opencode/copilot call as a stage: smoke → JSON envelope → sonnet fallback; no `agent` = the router's pick at `model` (default sonnet); returns `{ok, agent, answer, provider, model, error, status?, transport?, skipped?, fallback?}` (`fallback` = the original agent's name, when the sonnet fallback ran as `claude`) | 1, 6, 8 | `{agent?, model?, vendorModel?, profile?, cwd, task, smoke?, fallback?, tools?, timeoutMin?, agents?}` — `vendorModel` = `ask --model`, `profile` = `ask --profile` (turns the sonnet fallback off) — `agents` = agents-file text, heredoc'd like the task and passed to `ask --agents <file>` |
| `code-evaluator` | evaluate-code (`../../evaluate-code/references/eval.md`) | Sequential then parallel: each (scenario, condition, seed) trap run executes a mid-tier agent with vs without the skill under test, then judges score every run; returns `{runs, cells}`. Needs the scratch `base` laid out first (eval.md) | 1, 2 | `{base, skill, scenarios?, seeds?, model?, conditions?}` |

## Run from a leaf

A leaf has no `Workflow` tool, but a headless Claude session does — so a leaf runs a saved Workflow through one:

```bash
claude-code ask --agent claude --tools --timeout 20m \
  "Run Workflow({name: \"<name>\", args: {…}}) and print the result"
```

Only when the brief names it (`Workflow:` field, with args); `--background` past ~3 minutes, then `status`/`result`;
from a scratch cwd when the script writes files (the eval: `skills/evaluate-code/references/eval.md`). A headless
session reads only under its cwd, so from a scratch cwd pass the script body inline (`Workflow({script: <contents>})`)
instead of a `~/.claude/...` `scriptPath`. The job id goes on the leaf's `Ran on:` line, and the Workflow's result is
reported as a claim, never as the leaf's own verification.
