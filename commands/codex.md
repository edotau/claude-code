---
description: "Adversarial code review via the codex agent runner — second-opinion review using a different model stack to catch blind spots the session model misses. Writes a session-correlated handoff file."
argument-hint: "[--session-id <id>] [--uncommitted | --base <branch> | --commit <sha> | --pr] [extra review focus]"
allowed-tools: Bash
---

# /codex — Adversarial Review via Codex

Launcher only. Scope flags, session correlation, and the handoff/status file contract live in
`agents/codex.md` (REVIEW mode) and the script's own header comment.

Run the script with the Bash tool, passing `$ARGUMENTS` as ONE single-quoted argument (escape any `'` inside it):
`bash "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/scripts/codex-adversarial-review.sh" '<$ARGUMENTS>'`. In a worktree,
export `CLAUDE_CONFIG_DIR="$PWD"` first to run that checkout's copy of the script.

Relay the script's output, including the `SESSION_ID=` line and handoff/status paths.
