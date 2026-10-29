---
paths:
  - "internal/hooks/**"
---
# Guardrails — Evidence-Gated Behavior

Instructed behavior is hope; **gated behavior is checked**. The CLAUDE.md behavioral
guidelines are prose — these hooks are their enforcement layer. The pattern every gate follows:

1. **Trigger on a checkable condition** (transcript facts, file state — never self-report).
2. **Block with instructions AND the evidence contract** (what artifact will prove compliance).
3. **Release only on evidence** — artifact mtimes, receipts, exit codes; never on trust.
4. **Cap and fail open** — a gate that can trap a session is worse than no gate.

## Registered hooks and supporting watcher

Trigger and action per hook: `hooks.md` (the event → hook matrix; hooks run as groups behind one dispatcher
entry per event, switchable via `harnessHooks` except the safety gate); this table carries only the
evidence contract.

| Gate | Event | Evidence that releases it |
|---|---|---|
| `memory-harvest` / `memory-end` | Stop / SessionEnd | memory-bank mtime after the prompt (`activeContext`/`progress`/`sessionHistory.md`), or an explicit receipt |
| `memory watch` (spawned by `memory-harvest`) | detached, per session | the session stamp (`state/harness/memory/stamp-<id>`) carries the covered count; exits when the owner pid or transcript is gone |
| `memory-harvest` | Stop | once-per-session context checkpoint (the model writes a handoff (`claude-code memory update`), then stops); then memory harvest; then formats edited files and continues |
| `format-stop` | Stop | n/a — format group: formats files edited this turn, then continues; a repo with its own pre-commit switches it off |
| `safety-scan` | PreToolUse (Bash) | n/a — a block tier stops the call outright, an ask tier asks the user (codex and copilot have no ask, so it blocks there), a warn tier only prints; each block is logged to `claude-code doctor --audit` by reason and pattern, never the command. Safety gate group: own process, never switched off |
| `rtk-rewrite` | PreToolUse (Bash) | n/a — its own group: an eligible noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) is prefixed with the `rtk` proxy when it is on PATH (Claude sessions only, never `git` inside a linked worktree; `HARNESS_RTK_BIN` overrides the binary) |
| `docs-placement` | PreToolUse (Write\|Edit) | n/a — blocks the call; placement names the canonical home (`HARNESS_DOCS_PLACEMENT=0` disables); deny-one-call, not a Stop loop. Docs group, inside the PreToolUse dispatcher |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | the first write to a native plan draft is retried with `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md` (the denial pastes the skeleton) |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | n/a — advisory; mirrors to a dated, repo-tagged file, preserving the first destination/date across edits |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | n/a — advisory; files approved text at the mirror destination, adds a missing checklist, verifies the save, then removes the undated draft |
| `docs-plan-skeleton` / `docs-plan-skeleton-prompt` | PostToolUse (EnterPlanMode) / UserPromptSubmit | n/a — advisory; injects the `agents/Plan.md` skeleton when plan mode starts, once per plan on a plan-mode prompt |
| `docs-plan-tick` / `docs-plan-reminder` | PostToolUse (Bash) / Stop | n/a — advisory; a landed commit ticks the plan tasks its `owns:` covers, and partly landed tasks are reported once at Stop |
| `dispatch-effort` | PreToolUse (Agent\|Task\|Workflow) | the cap check blocks (below); otherwise never blocks; fills the model from a local prompt classifier (an explicit `model:` always wins), downshifts an unset model at ≥90% Anthropic plan pressure and moves `code-worker`/`Explore` dispatches to a coworker at ≥97% (`HARNESS_ROUTE_SUBAGENTS=0` disables); appends up to 3 memory recall pointers to the spawn prompt; on a Workflow it fills `model: 'sonnet'` via `updatedInput` (an explicit pin always wins), blocks only an options argument it cannot rewrite, a bad pin literal, or an unpinned `scriptPath` file, and redirects a saved-by-name call to `scriptPath` (the by-name registry is read once at session start) |
| dispatch cap (in `dispatch-effort`) | PreToolUse (Agent\|Task\|Workflow) | Wait for a running dispatch to retire or work sequentially; cap is 12 per available account (a profile's `concurrency` overrides it), total `12 × accounts` clamped to 4 × CPUs; infra errors fail open |
| `dispatch-failure` / `dispatch-workflow` | PostToolUseFailure (Agent\|Task\|Workflow) / PostToolUse (Workflow) | n/a — retire that dispatch's slots |
| `dispatch-stop` | SubagentStop | n/a — never blocks; retires the stopping subagent's slot |
| `memory-recall` | UserPromptSubmit | n/a — never blocks; injects `additionalContext` only; logs the entries to `access.log` |
| `memory-start` | SessionStart | n/a — injects catalog/focus memory and repairs the native memory link |
| `paths-resolve` | UserPromptSubmit | n/a — reports resolved paths; translates Windows paths to `/mnt/…` on WSL only |
| `memory-compact` | PreCompact | n/a — advisory background harvest; transcript survives compaction |
| `worktree-create` | WorktreeCreate | Creates at `~/worktrees/<repo>/<name>`; reports creation errors; opens the ledger `memory/<repo>/worktrees/<name>.md` |
| `worktree-remove` | WorktreeRemove | Refuses dirty trees; removes without force, deletes only a merged branch |

## Authoring a new gate

- Put the check in Go (`internal/hooks/`), not prose — prose is the instruction, the hook is
  the guarantee. Fail open on infra errors (missing transcript, unreadable file).
- A gate that can block indefinitely needs a cap and a fail-open path; announce the fail-open
  so it's visible, not silent.
- Test the block path, the evidence-release path, AND the fail-open cap.

## Related

- `hooks.md` — the event → hook matrix this gate inventory draws from
- `change-protocol.md` — the Impact Checklist a hook change must satisfy
