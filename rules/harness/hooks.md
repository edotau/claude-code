---
paths:
  - "internal/hooks/**"
  - "internal/hookspec/**"
---
# Hooks System

Lifecycle hooks run natively as `claude-code hook <name>`, declared in `internal/hookspec/`
and implemented in `internal/hooks/`.

**The authoring surface is `internal/hookspec/hookspec.go`** — one `Spec` per hook invocation,
and the single place a hook is declared. To add, remove, or retime a hook: edit `hookspec.go` +
the Go handler in `internal/hooks/`, then `make install` to render the updated chain into
`settings.json`. **Never hand-edit `settings.json`** → `settings-management.md`.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `bash-guard` | PreToolUse (Bash) | Blocks destructive shapes: `rm -rf /`, force-push to main, `curl \| sh`, `sudo`, `DROP TABLE`, `git stash/checkout/reset/restore/clean` aimed at `~/.claude`, …; a linked worktree under `.claude/worktrees/` is exempt. Then routes a noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) through the `rtk` output compressor when it is on PATH (Claude sessions only); machine-readable flags (`--porcelain`, `--json`, …) and value subcommands (`rev-parse`, …) bypass it. `HARNESS_RTK_BIN` overrides the binary |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a git-ignored `docs/`, naming the canonical home. `HARNESS_DOCS_PLACEMENT=0` disables |
| `plan-template` | PreToolUse (Write) | Denies the first write to an undated native plan draft without `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md` |
| `agent-effort` | PreToolUse (Agent\|Task) | Fills an UNSET subagent model from a local prompt classifier (low→haiku, else sonnet). No env override — an explicit `model:` on the dispatch or in the agent's frontmatter (`inherit` included = session model) always wins |
| `workflow-lint` | PreToolUse (Workflow) | Pins `model: 'sonnet'` on every unpinned `agent()` in an inline Workflow script (an unpinned leaf would inherit the session model); blocks an options argument it cannot rewrite, a bad pin literal, or a `scriptPath` file with an unpinned call. A saved-by-name call is redirected to `scriptPath` pointing at the on-disk file and linted the same way, since the by-name registry is read once at session start and would otherwise run a stale pre-edit copy |
| `agent-inflight` | PreToolUse (Agent\|Task) | Prunes stale markers, blocks at 8 machine-wide dispatches, and records a new marker; infra errors fail open |
| `agent-inflight-tag` | PostToolUse (Agent\|Task) | Binds a dispatch marker to the returned agent id, or retires a completed foreground spawn |
| `agent-inflight-post` | PostToolUseFailure (Agent\|Task) | Retires the failed spawn's marker |
| `subagent-stop` | SubagentStop | Sends a source-editing agent back for one simplify pass, then retires its inflight slot even if that stop was blocked |
| `plan-mirror` | PostToolUse (Write\|Edit) | Mirrors native undated drafts into dated, repo-tagged files from the first write; edits keep the same destination and date |
| `save-plan` | PostToolUse (ExitPlanMode) | On approval, reuses the mirror destination (or files a response-only plan), adds a checklist if absent, verifies the save, then removes the undated draft |
| `context-checkpoint` | Stop | Once per session at ≥85% context, blocks Stop so the model can hand off (via `/harness:adapt`) before compaction |
| `stop-format` | Stop | Formats files edited this turn: the repo's own `make format\|fmt` target when it honors a per-file variable (`TARGET`/`FILE`/…), else the pyproject-declared python chain, else gofmt/ruff/prettier/shfmt when installed |
| `session-start` | SessionStart | Injects the `memory/index.md` catalog + this repo's focus sections (Current Focus/Blockers/Next Steps, progress In Progress); repairs the cwd's `projects/<slug>/memory` link; Claude Code loads the repo's `MEMORY.md` itself |
| `wsl-path-normalize` | UserPromptSubmit | Reports existing file paths; maps pasted or dragged Windows paths to `/mnt/…` equivalents on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 3 blocks from this repo's store + global matching ≥2 prompt terms, plus any repo store the prompt names explicitly; skips blocks already in context |
| `access-trace` | PostToolUse (Read) | Logs a model read of a bank file (or its index) to that bank's `access.log`, the hot/cold signal `memory doctor` nominates from; never blocks |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Replace Claude Code's git logic: `--worktree`, `EnterWorktree` and subagent isolation create at `~/worktrees/<repo>/<name>` (Orca's root too); remove refuses a dirty tree and `branch -d`s the merged branch. Live payload carries only `name` |
| `pre-compact` | PreCompact | Before Claude Code compacts the context, dispatches the background harvest when the transcript has edits no earlier harvest covered (ADK `add_events_to_memory`); never blocks — the transcript file survives compaction |
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | After enough edits, spawns a detached `claude-code memory update` that runs the equivalent of `/memory:end`. The first dispatch also claims one detached `memory watch` per session: once the transcript is quiet 10 min with uncovered edits it harvests the final state, so a crash or kill (no SessionEnd) loses nothing |

## Notes

- Hook exit codes and stdin/stdout follow the Claude Code hook contract; an async hook does not
  block the turn.
- Permission guardrails live in the template's `permissions` block (`ask` beats `allow`),
  regenerated into `settings.json`.
- Stop/SubagentStop gates are bounded; deny-one-call gates reject the offending input and let
  the model retry corrected input (see `guardrails.md`).

## Related

- `settings-management.md` — the template is the source of truth for `settings.json`
- `guardrails.md` — the gate inventory and its evidence contract
