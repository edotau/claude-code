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

| Gate | Event | Trigger | Evidence that releases it |
|---|---|---|---|
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | Enough file edits in the transcript | memory-bank mtime after the prompt (`activeContext`/`progress`/`sessionHistory.md`), or an explicit receipt |
| `memory watch` (spawned by `session-harvest`) | detached, per session | Transcript quiet ≥10 min with edits no dispatch covered | the session stamp (`state/harness/memory/stamp-<id>`) carries the covered count; exits when the owner pid or transcript is gone |
| `context-checkpoint` | Stop | Context usage ≥ 85% of the model's real window | once-per-session (the model runs `/harness:adapt`, then stops) |
| `bash-guard` | PreToolUse (Bash) | A destructive command shape (`rm -rf /`, `sudo`, `curl \| sh`, `git stash/checkout/reset/restore/clean` aimed at the harness checkout, …); a linked worktree under `.claude/worktrees/` is exempt | n/a — blocks the call outright; each block is logged to `claude-code doctor --audit` by reason and pattern, never the command; an eligible noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) is then prefixed with the `rtk` proxy when it is on PATH (Claude sessions only; `HARNESS_RTK_BIN` overrides the binary) |
| `docs-placement` | PreToolUse (Write\|Edit) | A new `.md` targets a repo root or a git-ignored `docs/` dir, or the first write to an undated native plan draft lacks a required heading | n/a — blocks the call; placement names the canonical home (`HARNESS_DOCS_PLACEMENT=0` disables), plan-template is retried with `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md`; deny-one-call, not a Stop loop |
| `plan-mirror` | PostToolUse (Write\|Edit) | A native undated plan draft was written or edited | n/a — advisory; mirrors to a dated, repo-tagged file, preserving the first destination/date across edits |
| `save-plan` | PostToolUse (ExitPlanMode) | A plan was approved | n/a — advisory; files approved text at the mirror destination, adds a missing checklist, verifies the save, then removes the undated draft |
| `agent-effort` | PreToolUse (Agent\|Task) | A subagent dispatch with no `model:` set | n/a — never blocks; fills the model from a local prompt classifier (an explicit `model:` always wins) |
| `agent-inflight` | PreToolUse (Agent\|Task) | 8 dispatches already in flight, machine-wide | Wait for a slot to retire or work sequentially; stale/dead markers are pruned, infra errors fail open |
| `agent-inflight-tag` | PostToolUse (Agent\|Task) | A spawn returns an agent id or a foreground result | n/a — binds the marker to the agent id, or retires a completed foreground spawn |
| `agent-inflight-post` | PostToolUseFailure (Agent\|Task) | A spawn failed | n/a — retires that dispatch's marker |
| `subagent-stop` | SubagentStop | A source-editing subagent reaches its first stop | One simplify pass requested; the next stop proceeds via a persistent marker (read-only/config agent exemptions); inflight slot retires even on the blocked stop |
| `workflow-lint` | PreToolUse (Workflow) | An inline script with an `agent()` that pins no `model:` (an `agentType` alone keeps that type's own tier), or a `scriptPath` whose script does | fills `model: 'sonnet'` via `updatedInput` (an explicit pin always wins); blocks only an options argument it cannot rewrite, a bad pin literal, or an unpinned `scriptPath` file; a saved-by-name call is redirected to `scriptPath` (the on-disk file) via `updatedInput` and gated identically, since the by-name registry is read once at session start |
| `memory-recall` | UserPromptSubmit | Prompt carries ≥2 distinct terms of ≥3 chars matching a bank/native-memory block | n/a — never blocks; injects `additionalContext` only |
| `stop-format` | Stop | Any file edited this turn that a known formatter covers | n/a — never blocks; formats and continues |
| `access-trace` | PostToolUse (Read) | The read path is a typed bank file or its index | n/a — never blocks; appends one `access.log` line |
| `session-start` | SessionStart | Session begins | n/a — injects catalog/focus memory and repairs the native memory link |
| `wsl-path-normalize` | UserPromptSubmit | Prompt names an existing file | n/a — reports resolved paths; translates Windows paths to `/mnt/…` on WSL only |
| `pre-compact` | PreCompact | Transcript has edits no earlier harvest covered | n/a — advisory background harvest; transcript survives compaction |
| `worktree-create` | WorktreeCreate | Native worktree requested | Creates at `~/worktrees/<repo>/<name>`; reports creation errors |
| `worktree-remove` | WorktreeRemove | Native worktree removal requested | Refuses dirty trees; removes without force, deletes only a merged branch |

## Authoring a new gate

- Put the check in Go (`internal/hooks/`), not prose — prose is the instruction, the hook is
  the guarantee. Fail open on infra errors (missing transcript, unreadable file).
- A gate that can block indefinitely needs a cap and a fail-open path; announce the fail-open
  so it's visible, not silent.
- Test the block path, the evidence-release path, AND the fail-open cap.

## Related

- `hooks.md` — the event → hook matrix this gate inventory draws from
- `change-protocol.md` — the Impact Checklist a hook change must satisfy
