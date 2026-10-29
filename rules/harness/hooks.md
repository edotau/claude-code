---
paths:
  - "internal/hooks/**"
  - "internal/hookspec/**"
---
# Hooks System

Lifecycle hooks run natively as `claude-code hook <name>`, declared in `internal/hookspec/`
and implemented in `internal/hooks/`.

**The authoring surface is `internal/hookspec/hookspec.go`** — one `Spec` per hook invocation,
and the single place a hook is declared. To add, remove, or retime a hook: edit its group in `hookspec.go` +
the Go handler in `internal/hooks/`, then `make install` to render the updated chain into
`settings.json`. **Never hand-edit `settings.json`** → `settings-management.md`.

## Groups and the dispatcher

`hookspec.Registry` lists **groups** in execution order, each naming the events it handles. Settings carry **one
dispatcher entry per event** (`claude-code hook <Event>`, e.g. `hook PreToolUse --no-own`) whose matcher is the union
of its groups' matchers; the dispatcher reads the payload once, runs every enabled group's handlers within their
budgets, recovers a panic, and merges the results. Groups, in order: `safety`, `rtk`, `plans`, `dispatch`, `paths`,
`memory`, `format`, `worktree`, `codex` (codex-only; never rendered into Claude settings).

- **Safety exception:** the `safety` gate group (`safety-scan` and `docs-placement`, both PreToolUse) keeps its own
  process and settings entry per handler, so a hung or crashing group can never delay or skip a block. It cannot be
  switched off.
- **Switches:** every other group is on by default; turn one off with `"<group>": false` under `harnessHooks` in the
  vault overlay (`claude-code vault edit`). An unknown group or a gate group is rejected.
- **Vendors:** codex and copilot run only the handlers that list them (`safety-scan`, `docs-placement`, `plan-gate`,
  `path-resolver`, `memory-recall`). Neither has an ask decision, so a safety ask blocks there.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | Three tiers. **Block:** destructive shapes (`rm -rf /`, force-push to main, `curl \| sh`, `git stash/checkout/reset/restore/clean` aimed at `~/.claude`, a `git commit` carrying an AI-attribution footer in any repo, …); a linked worktree under `.claude/worktrees/` is exempt. **Ask:** intent-dependent shapes such as `sudo` and `DROP TABLE`, with the safer alternative named; codex and copilot have no ask, so there it blocks and says so. **Warn:** prints a warning, never blocks. Each block is logged to `claude-code doctor --audit` by reason and pattern, never the command |
| `rtk` | PreToolUse (Bash) | Its own group: routes a noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) through the `rtk` output compressor when it is on PATH (Claude sessions only; never `git` inside a linked worktree, whose isolation check refuses a wrapped git); machine-readable flags (`--porcelain`, `--json`, …) and value subcommands (`rev-parse`, …) bypass it. `HARNESS_RTK_BIN` overrides the binary |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a git-ignored `docs/`, naming the canonical home (`HARNESS_DOCS_PLACEMENT=0` disables). Safety gate group |
| `plan-gate` | PreToolUse (Write\|Edit) | Plans group: denies the first write to an undated native plan draft without `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md`, and pastes the skeleton in the denial |
| `agent-effort` | PreToolUse (Agent\|Task\|Workflow) | Dispatch group's single PreToolUse runner, in order: the cap (next row), the model pin, then the slot claim. Fills an UNSET subagent model from a local prompt classifier (low→haiku, else sonnet). An explicit `model:` on the dispatch or in the agent's frontmatter (`inherit` included = session model) always wins. Under Anthropic plan pressure it downshifts an unset model one step at ≥90% and moves a `code-worker`/`Explore` dispatch to a coworker at ≥97%; `HARNESS_ROUTE_SUBAGENTS=0` turns that rewrite off. Appends up to 3 memory recall pointers to the spawn prompt. On a Workflow it also pins `model: 'sonnet'` on every unpinned `agent()` in an inline script or `scriptPath` file (an unpinned leaf would inherit the session model), redirects a saved-by-name call to its on-disk `scriptPath` (the by-name registry is read once at session start), and blocks an options argument it cannot rewrite or a bad pin literal |
| (dispatch cap) | within `agent-effort` | One limiter gates every spawn: the cap is **B = 12 per available account** (a profile's `concurrency` overrides it), the machine-wide total is `B × accounts`, clamped to **4 × CPUs**. A blocked dispatch claims nothing; a Workflow claims one slot per `agent()` site. The block names the account, its cap and the total; infra errors fail open. `claude-code doctor` shows the live count as the `in-flight` row. Name the agent in a dispatch description as `<agent>: <desc>` |
| `agent-inflight-post` | PostToolUseFailure (Agent\|Task\|Workflow) | Retires the failed spawn's slots by `tool_use_id` |
| `workflow-inflight` | PostToolUse (Workflow) | Releases a finished Workflow's slots |
| `subagent-stop` | SubagentStop | Retires the stopping subagent's inflight marker |
| `plan-mirror` | PostToolUse (Write\|Edit) | Mirrors native undated drafts into dated, repo-tagged files from the first write, named from the plan's `Suggested slug:` line (else its H1); edits keep the same destination and date |
| `save-plan` | PostToolUse (ExitPlanMode) | On approval, reuses the mirror destination (or files a response-only plan), adds a checklist if absent, verifies the save, then removes the undated draft |
| `plan-skeleton` / `plan-skeleton-prompt` | PostToolUse (EnterPlanMode) / UserPromptSubmit | Injects the `agents/Plan.md` skeleton when plan mode starts and, once per plan, on a plan-mode prompt |
| `plan-tick` | PostToolUse (Bash) | A landed `git commit` ticks the active plan's tasks whose `owns:` it covers; partial coverage is noted. `claude-code plan status` shows ticked/total, task claims and partly-landed notes |
| `plan-reminder` | Stop | Reports once, advisory, the commits that only partly landed a task |
| `stop` | Stop | Memory group: runs the context checkpoint (once per session at ≥85% context, blocks Stop so the model can write a handoff with `claude-code memory update`), then the memory harvest (below). The harvest counts edits and user turns (2 turns weigh as 1 edit), so a conversation-only session is saved too |
| `stop-format` | Stop | Format group: formats files edited this turn: the repo's own `make format\|fmt` target when it honors a per-file variable (`TARGET`/`FILE`/…), else the pyproject-declared python chain, else gofmt/ruff/prettier/shfmt when installed; a repo with its own pre-commit switches the group off |
| `session-start` | SessionStart | Injects the `memory/index.md` catalog + this repo's focus sections (Current Focus/Blockers/Next Steps, progress In Progress); repairs the cwd's `projects/<slug>/memory` link; Claude Code loads the repo's `MEMORY.md` itself |
| `path-resolver` | UserPromptSubmit | Reports existing file paths; resolves paths in the prompt both ways through the mount table (Windows ⇄ `/mnt/…`) |
| `memory-recall` | UserPromptSubmit | Injects up to 3 blocks from this repo's store + home matching ≥2 prompt terms, plus any repo store the prompt names explicitly; skips blocks already in context; each injected entry is logged to the bank's `access.log` (as are `memory search` and `memory show` hits), the hot/cold signal `memory review` nominates from |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Replace Claude Code's git logic: `--worktree`, `EnterWorktree` and subagent isolation create at `~/worktrees/<repo>/<name>` (Orca's root too); remove refuses a dirty tree and `branch -d`s the merged branch. Live payload carries only `name`. Create honors `worktree.baseRef` and seeds `.venv` / `.env` from the main checkout. Each worktree has a ledger at `memory/<repo>/worktrees/<name>.md`, opened on create or adopt, logged as work proceeds, and closed on `worktree merge`, hook removal or `clean --force` with the commits it carried |
| `pre-compact` | PreCompact | Before Claude Code compacts the context, dispatches the background harvest when the transcript has edits no earlier harvest covered (ADK `add_events_to_memory`); never blocks — the transcript file survives compaction |
| `session-harvest-end` | SessionEnd | After enough edits, spawns a detached `claude-code memory update`. The first dispatch also claims one detached `memory watch` per session: once the transcript is quiet 10 min with uncovered edits it harvests the final state, so a crash or kill (no SessionEnd) loses nothing |

## Notes

- Hook exit codes and stdin/stdout follow the Claude Code hook contract; an async hook does not
  block the turn.
- Permission guardrails live in the repo defaults' `permissions` block (`ask` beats `allow`),
  regenerated into `settings.json`.
- Stop gates are bounded; deny-one-call gates reject the offending input and let
  the model retry corrected input (see `guardrails.md`).

## Related

- `settings-management.md` — the repo defaults are the source of truth for `settings.json`
- `guardrails.md` — the gate inventory and its evidence contract
