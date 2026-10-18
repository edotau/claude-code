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

## Gate inventory

| Gate | Event | Trigger | Evidence that releases it |
|---|---|---|---|
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | Enough file edits in the transcript | memory-bank mtime after the prompt (`activeContext`/`progress`/`sessionHistory.md`), or an explicit receipt |
| `memory watch` (spawned by `session-harvest`) | detached, per session | Transcript quiet ≥10 min with edits no dispatch covered | the session stamp (`state/harness/memory/stamp-<id>`) carries the covered count; exits when the owner pid or transcript is gone |
| `context-checkpoint` | Stop | Context usage ≥ 85% of the model's real window | once-per-session (the model runs `/harness:adapt`, then stops) |
| `safety` | PreToolUse (Bash) | A destructive command shape (`rm -rf /`, `sudo`, `curl \| sh`, `git stash/checkout/reset/restore/clean` aimed at the harness checkout, …); a linked worktree under `.claude/worktrees/` is exempt | n/a — blocks the call outright; each block is logged to `claude-code doctor --audit` by reason and pattern, never the command |
| `rtk-wrap` | PreToolUse (Bash) | A noisy command (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) with `rtk` on PATH | n/a — never blocks; rewrites argv to pipe through `rtk` (`HARNESS_RTK_BIN` overrides the binary) |
| `docs-placement` | PreToolUse (Write\|Edit) | A new `.md` targets a repo root or a git-ignored `docs/` dir | n/a — blocks the call, names the canonical home (`HARNESS_DOCS_PLACEMENT=0` disables) |
| `agent-effort` | PreToolUse (Agent) | A subagent dispatch with no `model:` set | n/a — never blocks; fills the model from a local prompt classifier (an explicit `model:` always wins) |
| `workflow-lint` | PreToolUse (Workflow) | An inline script with an `agent()` that pins no `model:` (an `agentType` alone keeps that type's own tier), or a `scriptPath` whose script does | fills `model: 'sonnet'` via `updatedInput` (an explicit pin always wins); blocks only an options argument it cannot rewrite, a bad pin literal, or an unpinned `scriptPath` file; a saved-by-name call is redirected to `scriptPath` (the on-disk file) via `updatedInput` and gated identically, since the by-name registry is read once at session start |
| `memory-recall` | UserPromptSubmit | Prompt carries ≥2 distinct terms of ≥3 chars matching a bank/native-memory block | n/a — never blocks; injects `additionalContext` only |
| `stop-format` | Stop | Any file edited this turn that a known formatter covers | n/a — never blocks; formats and continues |
| `access-trace` | PostToolUse (Read) | The read path is a typed bank file or its index | n/a — never blocks; appends one `access.log` line |

## Authoring a new gate

- Put the check in Go (`internal/hooks/`), not prose — prose is the instruction, the hook is
  the guarantee. Fail open on infra errors (missing transcript, unreadable file).
- A gate that can block indefinitely needs a cap and a fail-open path; announce the fail-open
  so it's visible, not silent.
- Test the block path, the evidence-release path, AND the fail-open cap.

## Related

- `hooks.md` — the event → hook matrix this gate inventory draws from
- `change-protocol.md` — the Impact Checklist a hook change must satisfy
