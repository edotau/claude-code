# Saved Workflows (`workflows/*.js`)

Run one only when the user asks for it; the default dispatch is `/code-workers` ‖ `copilot`
(`rules/workflow/master-workflow.md` → Phase 6). Deterministic templates of the master-workflow phases, runnable by
name (`Workflow({name, args})`) or inline from another script via `workflow(name, args)`. Sequential = awaited stages,
Parallel = `parallel()`/`pipeline()`, Loop = a bounded `while` — control flow is code, never a model decision. Agents
work in the shared tree.

Placement: a script that another script calls by name, or that more than one skill uses, lives in `workflows/`;
otherwise it lives in its owning skill's `scripts/` and runs by `scriptPath`.

The scripts stay in `workflows/`: Claude Code resolves a by-name run and a nested `workflow(name)` there only (read
once at session start), and the `workflow-lint` hook redirects a by-name call to `workflows/<name>.js`. This file is
their one doc home; `claude-code workflow lint` fails when a script is missing from the table below or a row names a
script that does not exist, so a rename or move cannot go unrecorded.

Every `agent()` call pins `model:` (or `agentType:`) AND `effort:` — an unpinned leaf inherits the session's reasoning
tier; a call that deliberately omits `effort:` carries `// lint:opus-ok` on its closing line. `meta` is a pure literal
and nothing throws at top level, or the registry skips the script silently. Scripts are plain JavaScript (the Workflow
tool runs no TypeScript); `claude-code workflow lint` checks all of the above and `make test` gates the shipped scripts.

| Workflow | Owner skill | Shape | Phases | args |
|----------|-------------|-------|--------|------|
| `green-keeper-loop` | problem-solving (`../../problem-solving/references/verify.md` Gate Loop) | Loop: gate → fix in scope → re-run, ≤N; exits `green\|stalled\|capped\|error`. Per-call default 3 iterations (verify.md caps the whole task at 5). Many repos: one call per repo, in parallel | 8 | `{gate?, cwd?, scope, maxIterations?, fixesPerIteration?}` |
| `code-engineer` | subagent-workflows (the parallel `code-worker` ‖ `copilot` shape) | Parallel intel → implementer ‖ copilot → green-keeper-loop. Does not chain into `code-analyzer` | 2, 6, 8 | `{task, cwd?, scope, packages?, gate?, maxIterations?, implModel?}` |
| `code-analyzer` | evaluate-code (its fix stage follows problem-solving) | Sized scope → chunked auditors → batched refute → fix ‖ test per package → green-keeper-loop → re-audit touched files (≤rounds). Loops rounds in-script (one level of `workflow()` nesting only); exits `dry\|no-fix\|stalled\|nothing-changed\|blocked\|capped\|error` | 2, 6, 8 | `{range \| paths \| findings (none = working-tree diff), cwd?, packages?, dimensions?, chunkChars?, fix?, minFix?, gate?, maxIterations?, rounds?, refuters?}` |
| `code-agent` | subagent-workflows (`cost-routing.md`) | One codex/gemini/opencode/copilot call as a stage: smoke → JSON envelope → sonnet fallback | 1, 6, 8 | `{leg, cwd, task, smoke?, fallback?, tools?, timeoutMin?}` |

A skill-owned script runs by `scriptPath` only, never by name: the A/B eval is
`skills/evaluate-code/scripts/code-evaluator.js` (`skills/evaluate-code/references/eval.md`).
