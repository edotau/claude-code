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

Trigger and action per hook: `hooks.md` (the event → hook matrix); this table carries only the
evidence contract.

| Gate | Event | Evidence that releases it |
|---|---|---|
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | memory-bank mtime after the prompt (`activeContext`/`progress`/`sessionHistory.md`), or an explicit receipt |
| `memory watch` (spawned by `session-harvest`) | detached, per session | the session stamp (`state/harness/memory/stamp-<id>`) carries the covered count; exits when the owner pid or transcript is gone |
| `context-checkpoint` | Stop | once-per-session (the model writes a handoff (`claude-code memory update`), then stops) |
| `bash-guard` | PreToolUse (Bash) | n/a — blocks the call outright; each block is logged to `claude-code doctor --audit` by reason and pattern, never the command; an eligible noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) is then prefixed with the `rtk` proxy when it is on PATH (Claude sessions only, never `git` inside a linked worktree; `HARNESS_RTK_BIN` overrides the binary) |
| `docs-placement` | PreToolUse (Write\|Edit) | n/a — blocks the call; placement names the canonical home (`HARNESS_DOCS_PLACEMENT=0` disables), plan-template is retried with `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md`; deny-one-call, not a Stop loop |
| `plan-mirror` | PostToolUse (Write\|Edit) | n/a — advisory; mirrors to a dated, repo-tagged file, preserving the first destination/date across edits |
| `save-plan` | PostToolUse (ExitPlanMode) | n/a — advisory; files approved text at the mirror destination, adds a missing checklist, verifies the save, then removes the undated draft |
| `agent-effort` | PreToolUse (Agent\|Task) | n/a — never blocks; fills the model from a local prompt classifier (an explicit `model:` always wins) and logs the decision to `decisions.jsonl`, best-effort; appends up to 3 memory recall pointers to the spawn prompt |
| `agent-routed` | SubagentStart | n/a — never blocks; appends the actual launch model to `audit.jsonl`, reconciled against `agent-effort`'s decision log |
| `agent-inflight` | PreToolUse (Agent\|Task) | Wait for a slot to retire or work sequentially; stale/dead markers are pruned, infra errors fail open |
| `agent-inflight-tag` | PostToolUse (Agent\|Task) | n/a — binds the marker to the agent id, or retires a completed foreground spawn |
| `agent-inflight-post` | PostToolUseFailure (Agent\|Task) | n/a — retires that dispatch's marker |
| `subagent-stop` | SubagentStop | n/a — never blocks; retires the stopping subagent's inflight slot |
| `workflow-lint` | PreToolUse (Workflow) | fills `model: 'sonnet'` via `updatedInput` (an explicit pin always wins); blocks only an options argument it cannot rewrite, a bad pin literal, or an unpinned `scriptPath` file; a saved-by-name call is redirected to `scriptPath` (the on-disk file) via `updatedInput` and gated identically, since the by-name registry is read once at session start |
| `memory-recall` | UserPromptSubmit | n/a — never blocks; injects `additionalContext` only |
| `stop-format` | Stop | n/a — never blocks; formats and continues |
| `access-trace` | PostToolUse (Read) | n/a — never blocks; appends one `access.log` line |
| `session-start` | SessionStart | n/a — injects catalog/focus memory and repairs the native memory link |
| `wsl-path-normalize` | UserPromptSubmit | n/a — reports resolved paths; translates Windows paths to `/mnt/…` on WSL only |
| `pre-compact` | PreCompact | n/a — advisory background harvest; transcript survives compaction |
| `worktree-create` | WorktreeCreate | Creates at `~/worktrees/<repo>/<name>`; reports creation errors |
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
